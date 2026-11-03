// Package agent is the AMCO node agent: it applies rendered configurations
// to an AIX host and reports the result through node annotations.
package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"

	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

const (
	// StateFile records the rendered config the agent last applied, so the
	// next update knows which files it owns and can remove stale ones.
	StateFile = "/etc/amco/applied-config.json"
	// ForceFile, when present, makes the agent re-apply the desired config in
	// full (including service restarts) and clear a drift degradation. It is
	// the counterpart of the MCO's /run/machine-config-daemon-force.
	ForceFile = "/etc/amco/force"
	// BackupSuffix is appended to a pre-existing file AMCO takes over, so it
	// can be restored when AMCO stops managing the path.
	BackupSuffix = ".amco-orig"
)

// pruneDirs are directories whose empty subdirectories are removed with the
// last managed file in them (one per registry host).
var pruneDirs = []string{"/etc/containerd/certs.d"}

// Applied is the persisted record of what the agent wrote. Config is the last
// config applied in full (files and restarts); Pending is set while an apply
// is in progress, so a failed or interrupted apply is redone in full.
type Applied struct {
	Name    string           `json:"name"`
	Config  *rendered.Config `json:"config,omitempty"`
	Pending *rendered.Config `json:"pending,omitempty"`
}

// Owned returns the files the agent may have written, by path.
func (a *Applied) Owned() map[string]rendered.File {
	out := map[string]rendered.File{}
	if a == nil {
		return out
	}
	for _, c := range []*rendered.Config{a.Config, a.Pending} {
		if c == nil {
			continue
		}
		for _, f := range c.Files {
			out[f.Path] = f
		}
	}
	return out
}

// Host writes files under Root (normally "/"; tests and dry runs use a
// scratch directory).
type Host struct {
	Root string
}

func (h Host) path(p string) string { return filepath.Join(h.Root, filepath.FromSlash(p)) }

// LoadApplied returns the last applied config, or nil if none was recorded.
func (h Host) LoadApplied() (*Applied, error) {
	data, err := os.ReadFile(h.path(StateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a := &Applied{}
	if err := json.Unmarshal(data, a); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", StateFile, err)
	}
	if a.Config == nil && a.Pending == nil {
		return nil, fmt.Errorf("%s has no config", StateFile)
	}
	for _, c := range []*rendered.Config{a.Config, a.Pending} {
		if c == nil {
			continue
		}
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", StateFile, err)
		}
	}
	return a, nil
}

// SaveApplied records a as the applied config.
func (h Host) SaveApplied(a *Applied) error {
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return h.writeFile(StateFile, data, 0o600)
}

// ForceRequested reports whether the force file exists.
func (h Host) ForceRequested() bool {
	_, err := os.Stat(h.path(ForceFile))
	return err == nil
}

// ClearForce removes the force file.
func (h Host) ClearForce() error {
	err := os.Remove(h.path(ForceFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Apply writes and removes files according to plan. owned is the set of
// paths the previous config managed; any other existing file that is about to
// be overwritten is backed up first. The caller records the applied config
// with SaveApplied once the services have been restarted, so a failed restart
// is retried with the same plan.
func (h Host) Apply(plan rendered.Plan, owned map[string]rendered.File) error {
	for _, f := range plan.Write {
		if _, ok := owned[f.Path]; !ok {
			if err := h.backup(f.Path, f.Contents); err != nil {
				return err
			}
		}
		if err := h.writeFile(f.Path, []byte(f.Contents), os.FileMode(f.Mode)); err != nil {
			return err
		}
		klog.Infof("wrote %s", f.Path)
	}
	for _, p := range plan.Remove {
		if err := h.remove(p); err != nil {
			return err
		}
		klog.Infof("removed %s", p)
	}
	return nil
}

// writeFile replaces p atomically: the content goes to a temporary file in
// the same directory, is synced, and is renamed over p.
func (h Host) writeFile(p string, data []byte, mode os.FileMode) error {
	full := h.path(p)
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".amco-tmp-"+filepath.Base(full)+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // already renamed on success
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", p, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), full); err != nil {
		return fmt.Errorf("replacing %s: %w", p, err)
	}
	// Persist the rename; not every filesystem supports syncing a directory.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// backup keeps a copy of a file that existed before AMCO managed it. A file
// that already has the new contents was written by an earlier, interrupted
// apply and is not backed up. A backup left over from an earlier takeover is
// replaced: the file was changed after AMCO let go of it.
func (h Host) backup(p, newContents string) error {
	full := h.path(p)
	data, err := os.ReadFile(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(data) == newContents {
		return nil
	}
	st, err := os.Stat(full)
	if err != nil {
		return err
	}
	klog.Infof("backing up pre-existing %s to %s%s", p, p, BackupSuffix)
	return h.writeFile(p+BackupSuffix, data, st.Mode().Perm())
}

// remove deletes a file AMCO no longer manages, restoring its backup if there
// is one, and prunes the emptied registry host directory.
func (h Host) remove(p string) error {
	full := h.path(p)
	if _, err := os.Stat(full + BackupSuffix); err == nil {
		return os.Rename(full+BackupSuffix, full)
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	parent := path.Dir(p)
	for _, d := range pruneDirs {
		if strings.HasPrefix(parent, d+"/") {
			_ = os.Remove(h.path(parent)) // only succeeds when empty
		}
	}
	return nil
}

// Drift lists the managed files whose content or permissions differ from cfg.
func (h Host) Drift(cfg *rendered.Config) []string {
	var out []string
	for _, f := range cfg.Files {
		full := h.path(f.Path)
		st, err := os.Stat(full)
		if err != nil {
			out = append(out, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}
		if st.Mode().Perm() != os.FileMode(f.Mode).Perm() {
			out = append(out, fmt.Sprintf("%s: mode %o, expected %o", f.Path, st.Mode().Perm(), f.Mode))
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			out = append(out, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}
		if !bytes.Equal(data, []byte(f.Contents)) {
			out = append(out, fmt.Sprintf("%s: content changed", f.Path))
		}
	}
	return out
}
