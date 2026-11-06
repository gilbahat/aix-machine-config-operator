// Package agent is the AMCO node agent: it converges an AIX host to a
// rendered configuration, item by item, and reports the result through node
// annotations. It never reboots the host.
package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	// StateFile records what the agent applied, each item's baseline and
	// what waits for a reboot.
	StateFile = "/etc/amco/applied-config.json"
	// ForceFile, when present, makes the agent re-apply the desired config in
	// full (including service restarts and a failed bosboot) and clear a drift
	// degradation. It is the counterpart of the MCO's
	// /run/machine-config-daemon-force.
	ForceFile = "/etc/amco/force"
	// BackupDir holds the original content of files AMCO took over, at
	// BackupDir + <path>. Keeping backups out of the original directory
	// means nothing scans them by accident (an rc.d S* script, a conf.d
	// drop-in).
	BackupDir = "/etc/amco/backups"
	// BackupSuffix marked backups kept next to the file by agents before
	// host configuration existed; they are still restored.
	BackupSuffix = ".amco-orig"
)

// pruneDirs are directories whose empty subdirectories are removed with the
// last managed file in them (one per registry host).
var pruneDirs = []string{"/etc/containerd/certs.d"}

// Host does file operations under Root (normally "/"; tests and dry runs use
// a scratch directory).
type Host struct {
	Root string
}

func (h Host) path(p string) string { return filepath.Join(h.Root, filepath.FromSlash(p)) }

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

// backupPath is where a file's original content is kept.
func backupPath(p string) string { return BackupDir + p }

// legacyBackupPath is where older agents kept it.
func legacyBackupPath(p string) string { return p + BackupSuffix }

// removeFile deletes p and prunes an emptied registry host directory.
func (h Host) removeFile(p string) error {
	if err := os.Remove(h.path(p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
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
