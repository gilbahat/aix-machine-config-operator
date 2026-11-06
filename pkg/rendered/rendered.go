// Package rendered defines the configuration AMCO renders for a pool of AIX
// nodes. The operator produces a Config and stores it in an immutable
// ConfigMap named after the Config's content hash; the node agent reads it
// back and converges the host to it, item by item.
//
// A Config is a set of items, each identified by a key:
//
//	file:<path>                  a file (AMCO's own Kubernetes plumbing or user content)
//	subsystem:<name>             an SRC subsystem: its state and, optionally, its definition
//	inittab:<identifier>         one /etc/inittab entry
//	tunable:<command>:<name>     a no/vmo/ioo/schedo/nfso/raso tunable
//	device:<device>:<attribute>  a device attribute (chdev)
//
// The package is deliberately free of heavy dependencies so the agent can
// share it.
package rendered

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/gilbahat/aix-machine-config-operator/pkg/aix"
)

// FormatVersion is the version of the Config schema.
const FormatVersion = 2

// ConfigMapKey is the key of the serialized Config inside its ConfigMap.
const ConfigMapKey = "config.json"

// PoolLabel labels rendered ConfigMaps with the pool they were rendered for.
const PoolLabel = "amco.reuna-labs.io/pool"

// SRC subsystem names of the node services AMCO itself restarts.
const (
	SubsystemContainerd = "containerd"
	SubsystemKubelet    = "kubelet"
	// SubsystemAgent is the AMCO agent; configs may not manage it.
	SubsystemAgent = "amco-agent"
)

// Policy holds the per-item settings common to every item type.
type Policy struct {
	// Drain makes the operator cordon and drain the node before a change to
	// this item (adding, changing or removing it) is applied.
	Drain bool `json:"drain,omitempty"`
	// Required makes the item block the rest of the configuration when it
	// cannot converge in place: the node reports Degraded instead of
	// skipping the item and reporting ReprovisionRequired.
	Required bool `json:"required,omitempty"`
}

// Origin says who asked for a file.
type Origin string

// File origins.
const (
	// OriginAMCO files are AMCO's own Kubernetes plumbing (registries,
	// kubelet configuration, CA bundles); they may only live under the
	// paths in managedFiles and managedPrefixes.
	OriginAMCO Origin = "amco"
	// OriginUser files come from AIXMachineConfigs; they may live anywhere
	// that is not reserved.
	OriginUser Origin = "user"
)

// SubsystemAction is what to do to an SRC subsystem when an item changes.
type SubsystemAction struct {
	Subsystem string `json:"subsystem"`
	// Action is "restart" or "refresh".
	Action string `json:"action"`
	// IgnoreMissing skips the action when the subsystem is not defined on
	// the host. AMCO sets it for containerd and the kubelet, so a node can
	// be configured before they are installed.
	IgnoreMissing bool `json:"ignoreMissing,omitempty"`
}

// Subsystem actions.
const (
	ActionRestart = "restart"
	ActionRefresh = "refresh"
)

// File is one file AMCO manages on the node.
type File struct {
	// Path is absolute.
	Path string `json:"path"`
	// Contents is the file content; with Encoding "base64" it is base64.
	Contents string `json:"contents"`
	Encoding string `json:"encoding,omitempty"`
	// Mode holds the permission bits.
	Mode uint32 `json:"mode"`
	// Owner and Group are a user and group name or a numeric id; empty
	// means root (0) and system (0).
	Owner  string `json:"owner,omitempty"`
	Group  string `json:"group,omitempty"`
	Origin Origin `json:"origin"`
	// Actions run after the file changes, after all files are written.
	Actions []SubsystemAction `json:"actions,omitempty"`
	Policy  `json:",inline"`
}

// Data returns the decoded file content.
func (f File) Data() ([]byte, error) {
	if f.Encoding == "base64" {
		return base64.StdEncoding.DecodeString(f.Contents)
	}
	return []byte(f.Contents), nil
}

// Subsystem states.
const (
	SubsystemActive      = "active"
	SubsystemInoperative = "inoperative"
)

// Subsystem is an SRC subsystem's desired state and, optionally, definition.
type Subsystem struct {
	Name string `json:"name"`
	// State is SubsystemActive, SubsystemInoperative, or empty to leave
	// the state alone (for a definition-only item).
	State string `json:"state,omitempty"`
	// Definition, when set, is created with mkssys or enforced with chssys.
	Definition *aix.SubsystemDefinition `json:"definition,omitempty"`
	Policy     `json:",inline"`
}

// InittabEntry is one /etc/inittab entry managed with mkitab/chitab/rmitab.
type InittabEntry struct {
	aix.InittabEntry `json:",inline"`
	Policy           `json:",inline"`
}

// Tunable is one kernel tunable.
type Tunable struct {
	// Command is no, vmo, ioo, schedo, nfso or raso.
	Command string `json:"command"`
	Name    string `json:"name"`
	Value   string `json:"value"`
	Policy  `json:",inline"`
}

// What to do when a device is busy.
const (
	OnBusyDefer = "Defer"
	OnBusyFail  = "Fail"
)

// DeviceAttribute is one device attribute set with chdev.
type DeviceAttribute struct {
	Device    string `json:"device"`
	Attribute string `json:"attribute"`
	Value     string `json:"value"`
	// OnBusy is OnBusyDefer (default) or OnBusyFail.
	OnBusy string `json:"onBusy,omitempty"`
	Policy `json:",inline"`
}

// Config is the full configuration rendered for a pool.
type Config struct {
	Version          int               `json:"version"`
	Pool             string            `json:"pool"`
	Files            []File            `json:"files,omitempty"`
	Subsystems       []Subsystem       `json:"subsystems,omitempty"`
	Inittab          []InittabEntry    `json:"inittab,omitempty"`
	Tunables         []Tunable         `json:"tunables,omitempty"`
	DeviceAttributes []DeviceAttribute `json:"deviceAttributes,omitempty"`
}

// Item keys.

// FileKey is the item key of a file.
func FileKey(p string) string { return "file:" + p }

// SubsystemKey is the item key of a subsystem.
func SubsystemKey(name string) string { return "subsystem:" + name }

// InittabKey is the item key of an inittab entry.
func InittabKey(id string) string { return "inittab:" + id }

// TunableKey is the item key of a tunable.
func TunableKey(command, name string) string { return "tunable:" + command + ":" + name }

// DeviceKey is the item key of a device attribute.
func DeviceKey(device, attribute string) string { return "device:" + device + ":" + attribute }

// Key returns the item key.
func (f File) Key() string { return FileKey(f.Path) }

// Key returns the item key.
func (s Subsystem) Key() string { return SubsystemKey(s.Name) }

// Key returns the item key.
func (e InittabEntry) Key() string { return InittabKey(e.Identifier) }

// Key returns the item key.
func (t Tunable) Key() string { return TunableKey(t.Command, t.Name) }

// Key returns the item key.
func (d DeviceAttribute) Key() string { return DeviceKey(d.Device, d.Attribute) }

// Item is any configuration item.
type Item interface {
	Key() string
	policy() Policy
}

func (f File) policy() Policy            { return f.Policy }
func (s Subsystem) policy() Policy       { return s.Policy }
func (e InittabEntry) policy() Policy    { return e.Policy }
func (t Tunable) policy() Policy         { return t.Policy }
func (d DeviceAttribute) policy() Policy { return d.Policy }

// PolicyOf returns an item's common settings.
func PolicyOf(i Item) Policy { return i.policy() }

// Items returns every item of the config by key.
func (c *Config) Items() map[string]Item {
	out := map[string]Item{}
	if c == nil {
		return out
	}
	for _, f := range c.Files {
		out[f.Key()] = f
	}
	for _, s := range c.Subsystems {
		out[s.Key()] = s
	}
	for _, e := range c.Inittab {
		out[e.Key()] = e
	}
	for _, t := range c.Tunables {
		out[t.Key()] = t
	}
	for _, d := range c.DeviceAttributes {
		out[d.Key()] = d
	}
	return out
}

// managedFiles and managedPrefixes are the only places AMCO's own files may
// be written.
var (
	managedFiles = map[string]bool{
		"/etc/kubernetes/kubelet.conf":   true,
		"/etc/kubernetes/kubelet-ca.crt": true,
	}
	managedPrefixes = []string{
		"/etc/containerd/certs.d/",
		"/etc/containerd/conf.d/",
		"/var/ssl/certs/amco-",
	}
	// reservedPrefixes may not be written by user files: AMCO's own state
	// and binaries, and files that AMCO manages through commands instead
	// (inittab entries, tunables).
	reservedFiles    = map[string]bool{"/etc/inittab": true, "/etc/amco": true, "/opt/amco": true, "/etc/tunables": true}
	reservedPrefixes = []string{"/etc/amco/", "/opt/amco/", "/etc/tunables/"}
)

// IsManagedPath reports whether an AMCO-origin file may be written at p.
func IsManagedPath(p string) bool {
	if managedFiles[p] {
		return true
	}
	for _, pre := range managedPrefixes {
		if strings.HasPrefix(p, pre) && len(p) > len(pre) {
			return true
		}
	}
	return false
}

// IsReservedPath reports whether a user file may not be written at p: the
// paths AMCO renders itself, and AMCO's own state, binaries and the files it
// manages through commands.
func IsReservedPath(p string) bool {
	if IsManagedPath(p) || reservedFiles[p] {
		return true
	}
	for _, pre := range append(append([]string{}, managedPrefixes...), reservedPrefixes...) {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// AccountPattern matches a user or group name or a numeric id.
const AccountPattern = `^([a-z_][a-z0-9_.-]{0,31}|[0-9]{1,10})$`

var accountRE = regexp.MustCompile(AccountPattern)

// Validate checks every rule the agent relies on. The agent runs as root,
// so this (not the operator) is what bounds what a rendered config can do.
func (c *Config) Validate() error {
	if c.Version != FormatVersion {
		return fmt.Errorf("unsupported rendered config version %d (agent supports %d)", c.Version, FormatVersion)
	}
	seen := map[string]bool{}
	dup := func(k string) error {
		if seen[k] {
			return fmt.Errorf("item %s listed twice", k)
		}
		seen[k] = true
		return nil
	}
	for _, f := range c.Files {
		if err := validateFile(f); err != nil {
			return err
		}
		if err := dup(f.Key()); err != nil {
			return err
		}
	}
	for _, s := range c.Subsystems {
		if err := aix.ValidateSubsystemName(s.Name); err != nil {
			return err
		}
		if s.Name == SubsystemAgent {
			return fmt.Errorf("subsystem %s is AMCO's own agent and cannot be managed", s.Name)
		}
		switch s.State {
		case "", SubsystemActive, SubsystemInoperative:
		default:
			return fmt.Errorf("subsystem %s: state %q must be %s or %s", s.Name, s.State, SubsystemActive, SubsystemInoperative)
		}
		if s.State == "" && s.Definition == nil {
			return fmt.Errorf("subsystem %s: neither a state nor a definition is given", s.Name)
		}
		if s.Definition != nil {
			if err := s.Definition.Validate(); err != nil {
				return fmt.Errorf("subsystem %s: %w", s.Name, err)
			}
		}
		if err := dup(s.Key()); err != nil {
			return err
		}
	}
	for _, e := range c.Inittab {
		if err := aix.ValidateInittabEntry(e.InittabEntry); err != nil {
			return err
		}
		if e.Identifier == SubsystemAgent {
			return fmt.Errorf("inittab entry %s starts AMCO's own agent and cannot be managed", e.Identifier)
		}
		if err := dup(e.Key()); err != nil {
			return err
		}
	}
	for _, t := range c.Tunables {
		if err := aix.ValidateTunable(t.Command, t.Name, t.Value); err != nil {
			return err
		}
		if err := dup(t.Key()); err != nil {
			return err
		}
	}
	for _, d := range c.DeviceAttributes {
		if err := aix.ValidateDeviceAttribute(d.Device, d.Attribute, d.Value); err != nil {
			return err
		}
		switch d.OnBusy {
		case "", OnBusyDefer, OnBusyFail:
		default:
			return fmt.Errorf("device %s: onBusy %q must be %s or %s", d.Device, d.OnBusy, OnBusyDefer, OnBusyFail)
		}
		if err := dup(d.Key()); err != nil {
			return err
		}
	}
	return nil
}

func validateFile(f File) error {
	if !path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || f.Path == "/" || strings.ContainsAny(f.Path, "\x00\n") {
		return fmt.Errorf("file path %q must be absolute and clean", f.Path)
	}
	base := path.Base(f.Path)
	if strings.HasSuffix(base, ".amco-orig") || strings.HasPrefix(base, ".amco-tmp-") {
		return fmt.Errorf("file path %q uses a name AMCO keeps for its own backups", f.Path)
	}
	switch f.Origin {
	case OriginAMCO:
		if !IsManagedPath(f.Path) {
			return fmt.Errorf("file path %q is outside the locations AMCO manages", f.Path)
		}
	case OriginUser:
		if IsReservedPath(f.Path) {
			return fmt.Errorf("file path %q is reserved (AMCO renders it, keeps its own state there, or manages it through commands)", f.Path)
		}
	default:
		return fmt.Errorf("file %q has unknown origin %q", f.Path, f.Origin)
	}
	if f.Mode&^0o777 != 0 {
		return fmt.Errorf("file %q has invalid mode %o (only permission bits are allowed)", f.Path, f.Mode)
	}
	switch f.Encoding {
	case "":
	case "base64":
		if _, err := f.Data(); err != nil {
			return fmt.Errorf("file %q: invalid base64 contents: %w", f.Path, err)
		}
	default:
		return fmt.Errorf("file %q: unknown encoding %q", f.Path, f.Encoding)
	}
	for _, a := range []string{f.Owner, f.Group} {
		if a != "" && !accountRE.MatchString(a) {
			return fmt.Errorf("file %q: invalid owner or group %q", f.Path, a)
		}
	}
	for _, a := range f.Actions {
		if err := aix.ValidateSubsystemName(a.Subsystem); err != nil {
			return fmt.Errorf("file %q: %w", f.Path, err)
		}
		if a.Subsystem == SubsystemAgent {
			return fmt.Errorf("file %q: %s cannot be restarted by a config", f.Path, SubsystemAgent)
		}
		if a.Action != ActionRestart && a.Action != ActionRefresh {
			return fmt.Errorf("file %q: action %q must be %s or %s", f.Path, a.Action, ActionRestart, ActionRefresh)
		}
	}
	return nil
}

// Normalize sorts every list by item key so equal configs serialize identically.
func (c *Config) Normalize() {
	sort.Slice(c.Files, func(i, j int) bool { return c.Files[i].Path < c.Files[j].Path })
	sort.Slice(c.Subsystems, func(i, j int) bool { return c.Subsystems[i].Name < c.Subsystems[j].Name })
	sort.Slice(c.Inittab, func(i, j int) bool { return c.Inittab[i].Identifier < c.Inittab[j].Identifier })
	sort.Slice(c.Tunables, func(i, j int) bool { return c.Tunables[i].Key() < c.Tunables[j].Key() })
	sort.Slice(c.DeviceAttributes, func(i, j int) bool { return c.DeviceAttributes[i].Key() < c.DeviceAttributes[j].Key() })
}

// Marshal returns the canonical JSON encoding of the config.
func (c *Config) Marshal() ([]byte, error) {
	c.Normalize()
	return json.Marshal(c)
}

// Unmarshal decodes and validates a config.
func Unmarshal(data []byte) (*Config, error) {
	c := &Config{}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("decoding rendered config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c.Normalize()
	return c, nil
}

// Name returns the content-addressed name of the config, which is also the
// name of its ConfigMap: rendered-<pool>-<first 16 hex digits of sha256>.
func (c *Config) Name() (string, error) {
	data, err := c.Marshal()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("rendered-%s-%s", c.Pool, hex.EncodeToString(sum[:8])), nil
}

// Changes lists the keys of items that differ between from and to: added,
// changed or removed. A nil from means nothing is known about the node, so
// every item of to counts as changed.
func Changes(from, to *Config) []string {
	old, cur := from.Items(), to.Items()
	var keys []string
	for k, item := range cur {
		if o, ok := old[k]; !ok || !reflect.DeepEqual(o, item) {
			keys = append(keys, k)
		}
	}
	for k := range old {
		if _, ok := cur[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// DrainNeeded reports whether moving a node from one config to another
// changes an item that asks for a drain (in either config).
func DrainNeeded(from, to *Config) bool {
	old, cur := from.Items(), to.Items()
	for _, k := range Changes(from, to) {
		if i, ok := cur[k]; ok && i.policy().Drain {
			return true
		}
		if i, ok := old[k]; ok && i.policy().Drain {
			return true
		}
	}
	return false
}
