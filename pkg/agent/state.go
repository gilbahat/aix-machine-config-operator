package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"

	"github.com/gilbahat/aix-machine-config-operator/pkg/aix"
	"github.com/gilbahat/aix-machine-config-operator/pkg/nodestate"
	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

// State is the agent's persistent record (StateFile). It is saved before
// every change, so an interrupted apply can always be resumed or undone.
type State struct {
	// Name and Config are the last config applied in full.
	Name   string           `json:"name,omitempty"`
	Config *rendered.Config `json:"config,omitempty"`
	// Pending is set while an apply is in progress. A config left pending
	// is re-applied in full: every item is re-checked and every action of
	// every item is run again.
	Pending *rendered.Config `json:"pending,omitempty"`

	// Baselines hold each managed item's state from before AMCO first
	// managed it. When the item leaves the config it is restored to this.
	Baselines map[string]Baseline `json:"baselines,omitempty"`
	// PendingReboot holds items applied persistently that take effect at
	// the next boot, including restores of such items.
	PendingReboot map[string]PendingItem `json:"pendingReboot,omitempty"`
	// Reprovision lists the items of Config that could not converge in place.
	Reprovision []nodestate.ItemIssue `json:"reprovision,omitempty"`

	// BosbootNeeded is set when a Bosboot-type tunable was changed and the
	// boot image has not been rebuilt since.
	BosbootNeeded bool `json:"bosbootNeeded,omitempty"`
	// Bosboot is the result of the last bosboot run since the last boot.
	Bosboot *nodestate.BosbootStatus `json:"bosboot,omitempty"`
	// BosbootBoot is the boot during which Bosboot ran.
	BosbootBoot string `json:"bosbootBoot,omitempty"`
	// BosbootFailedFor names the config whose bosboot failed. It is not
	// retried automatically; a new config or the force file retries it.
	BosbootFailedFor string `json:"bosbootFailedFor,omitempty"`

	// Restarting lists subsystems AMCO stopped as part of a restart that
	// has not finished. They are started again even though they are not
	// running, so a failed restart never leaves a service down.
	Restarting []string `json:"restarting,omitempty"`
}

// PendingItem is a change waiting for the next boot.
type PendingItem struct {
	// Value is what the item is set to (tunable or device attribute value).
	Value string `json:"value"`
	// SetBoot is the boot during which the change was made (RFC 3339 boot
	// time). A different boot time means a reboot has happened since.
	SetBoot string `json:"setBoot"`
}

// Baseline is an item's original state. Exactly one field is set, for the
// item's kind.
type Baseline struct {
	File      *FileBaseline      `json:"file,omitempty"`
	Subsystem *SubsystemBaseline `json:"subsystem,omitempty"`
	Inittab   *InittabBaseline   `json:"inittab,omitempty"`
	Tunable   *ValueBaseline     `json:"tunable,omitempty"`
	Device    *ValueBaseline     `json:"device,omitempty"`
}

// FileBaseline records whether a file existed, and its mode and owner. The
// content of a file that existed is kept next to it as <path>.amco-orig.
type FileBaseline struct {
	Exists bool   `json:"exists"`
	Mode   uint32 `json:"mode,omitempty"`
	UID    int    `json:"uid,omitempty"`
	GID    int    `json:"gid,omitempty"`
}

// SubsystemBaseline records an SRC subsystem's definition and state.
type SubsystemBaseline struct {
	Defined    bool                     `json:"defined"`
	Definition *aix.SubsystemDefinition `json:"definition,omitempty"`
	State      string                   `json:"state,omitempty"`
}

// InittabBaseline records an inittab entry, or its absence.
type InittabBaseline struct {
	Entry *aix.InittabEntry `json:"entry,omitempty"`
}

// ValueBaseline records a tunable's or device attribute's value. For
// tunables Current is the running value and Boot the next-boot value.
type ValueBaseline struct {
	Current string `json:"current"`
	Boot    string `json:"boot,omitempty"`
}

// Owned returns every item the agent manages or was in the middle of
// applying, by key.
func (s *State) Owned() map[string]rendered.Item {
	out := map[string]rendered.Item{}
	if s == nil {
		return out
	}
	for k, v := range s.Config.Items() {
		out[k] = v
	}
	for k, v := range s.Pending.Items() {
		out[k] = v
	}
	return out
}

// PendingRebootKeys returns the pending item keys, sorted.
func (s *State) PendingRebootKeys() []string {
	var keys []string
	for k := range s.PendingReboot {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// LoadState returns the persisted state; an absent state file yields an
// empty state.
func (h Host) LoadState() (*State, error) {
	data, err := os.ReadFile(h.path(StateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var probe struct {
		Config  *struct{ Version int } `json:"config"`
		Pending *struct{ Version int } `json:"pending"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", StateFile, err)
	}
	if (probe.Config != nil && probe.Config.Version == 1) || (probe.Pending != nil && probe.Pending.Version == 1) {
		return h.loadV1State(data)
	}
	s := &State{}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", StateFile, err)
	}
	for _, c := range []*rendered.Config{s.Config, s.Pending} {
		if c == nil {
			continue
		}
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", StateFile, err)
		}
	}
	return s, nil
}

// loadV1State converts the state written by agents before host
// configuration existed: only AMCO's own files (root, 0644), whose
// pre-existing content was backed up as <path>.amco-orig.
func (h Host) loadV1State(data []byte) (*State, error) {
	type v1File struct {
		Path     string `json:"path"`
		Mode     uint32 `json:"mode"`
		Contents string `json:"contents"`
	}
	type v1Config struct {
		Pool  string   `json:"pool"`
		Files []v1File `json:"files"`
	}
	var old struct {
		Name    string    `json:"name"`
		Config  *v1Config `json:"config"`
		Pending *v1Config `json:"pending"`
	}
	if err := json.Unmarshal(data, &old); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", StateFile, err)
	}
	s := &State{Name: old.Name, Baselines: map[string]Baseline{}}
	convert := func(c *v1Config) *rendered.Config {
		if c == nil {
			return nil
		}
		out := &rendered.Config{Version: rendered.FormatVersion, Pool: c.Pool}
		for _, f := range c.Files {
			out.Files = append(out.Files, rendered.File{Path: f.Path, Mode: f.Mode, Contents: f.Contents, Origin: rendered.OriginAMCO})
			_, err := os.Stat(h.path(f.Path + BackupSuffix))
			s.Baselines[rendered.FileKey(f.Path)] = Baseline{File: &FileBaseline{Exists: err == nil, Mode: 0o644}}
		}
		return out
	}
	s.Config = convert(old.Config)
	s.Pending = convert(old.Pending)
	if s.Config != nil {
		// The v1 config is not a v2 config's content, so its name no
		// longer matches; force the next config to be applied.
		s.Pending, s.Config = mergeConfigs(s.Config, s.Pending), nil
	}
	return s, nil
}

func mergeConfigs(a, b *rendered.Config) *rendered.Config {
	if b == nil {
		return a
	}
	out := *b
	seen := map[string]bool{}
	for _, f := range b.Files {
		seen[f.Path] = true
	}
	for _, f := range a.Files {
		if !seen[f.Path] {
			out.Files = append(out.Files, f)
		}
	}
	return &out
}

// SaveState persists s.
func (h Host) SaveState(s *State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return h.writeFile(StateFile, data, 0o600)
}
