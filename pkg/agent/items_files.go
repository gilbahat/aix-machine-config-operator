package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"k8s.io/klog/v2"

	"github.com/gilbahat/aix-machine-config-operator/pkg/rendered"
)

func (r *run) wantOwner(f rendered.File) (int, int, error) {
	if r.e.DryRun {
		return 0, 0, nil
	}
	uid, err := r.uid(f.Owner)
	if err != nil {
		return 0, 0, fmt.Errorf("owner %q: %w", f.Owner, err)
	}
	gid, err := r.gid(f.Group)
	if err != nil {
		return 0, 0, fmt.Errorf("group %q: %w", f.Group, err)
	}
	return uid, gid, nil
}

func (r *run) checkFile(f rendered.File) (status, string, error) {
	want, err := f.Data()
	if err != nil {
		return inSync, "", err
	}
	uid, gid, err := r.wantOwner(f)
	if err != nil {
		return inSync, "", err
	}
	full := r.e.Files.path(f.Path)
	fi, err := os.Stat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return needsChange, "missing", nil
	}
	if err != nil {
		return inSync, "", err
	}
	if !fi.Mode().IsRegular() {
		return inSync, "", fmt.Errorf("%s exists and is not a regular file", f.Path)
	}
	if fi.Mode().Perm() != os.FileMode(f.Mode).Perm() {
		return needsChange, fmt.Sprintf("mode %o, want %o", fi.Mode().Perm(), f.Mode), nil
	}
	if fu, fg, ok := r.owner(full, fi); ok && (fu != uid || fg != gid) {
		return needsChange, fmt.Sprintf("owner %d:%d, want %d:%d", fu, fg, uid, gid), nil
	}
	got, err := os.ReadFile(full)
	if err != nil {
		return inSync, "", err
	}
	if !bytes.Equal(got, want) {
		return needsChange, "content changed", nil
	}
	return inSync, "", nil
}

func (r *run) baselineFile(f rendered.File) (Baseline, error) {
	fi, err := os.Stat(r.e.Files.path(f.Path))
	if errors.Is(err, fs.ErrNotExist) {
		return Baseline{File: &FileBaseline{}}, nil
	}
	if err != nil {
		return Baseline{}, err
	}
	b := &FileBaseline{Exists: true, Mode: uint32(fi.Mode().Perm())}
	b.UID, b.GID, _ = r.owner(r.e.Files.path(f.Path), fi)
	return Baseline{File: b}, nil
}

func (r *run) applyFile(f rendered.File) error {
	data, err := f.Data()
	if err != nil {
		return err
	}
	uid, gid, err := r.wantOwner(f)
	if err != nil {
		return err
	}
	// Keep the original content before the first change. If an earlier,
	// interrupted apply already wrote the file, the backup exists already.
	if b, ok := r.st.Baselines[f.Key()]; ok && b.File != nil && b.File.Exists {
		if _, err := os.Stat(r.e.Files.path(backupPath(f.Path))); errors.Is(err, fs.ErrNotExist) {
			orig, err := os.ReadFile(r.e.Files.path(f.Path))
			if err != nil {
				return fmt.Errorf("backing up %s: %w", f.Path, err)
			}
			if err := r.e.Files.writeFile(backupPath(f.Path), orig, os.FileMode(b.File.Mode)); err != nil {
				return fmt.Errorf("backing up %s: %w", f.Path, err)
			}
			klog.Infof("kept the original %s as %s", f.Path, backupPath(f.Path))
		}
	}
	if err := r.e.Files.writeFile(f.Path, data, os.FileMode(f.Mode)); err != nil {
		return err
	}
	if err := r.chown(r.e.Files.path(f.Path), uid, gid); err != nil {
		return fmt.Errorf("setting owner of %s: %w", f.Path, err)
	}
	klog.Infof("wrote %s", f.Path)
	r.queueActions(f)
	return nil
}

// restoreFile puts a file back as it was before AMCO managed it: the backup
// is moved back with its original mode and owner, or the file is removed if
// it did not exist. The file's actions run, since its content changed.
func (r *run) restoreFile(f rendered.File, b *FileBaseline, ok bool) error {
	full := r.e.Files.path(f.Path)
	backup := r.e.Files.path(backupPath(f.Path))
	_, backupErr := os.Stat(backup)
	switch {
	case ok && b != nil && !b.Exists:
		if err := r.e.Files.removeFile(f.Path); err != nil {
			return err
		}
		klog.Infof("removed %s (it did not exist before AMCO managed it)", f.Path)
	case backupErr == nil:
		if err := os.Rename(backup, full); err != nil {
			return err
		}
		if ok && b != nil {
			if err := os.Chmod(full, os.FileMode(b.Mode)); err != nil {
				return err
			}
			if err := r.chown(full, b.UID, b.GID); err != nil {
				return err
			}
		}
		klog.Infof("restored the original %s", f.Path)
	case !ok && f.Origin == rendered.OriginAMCO:
		// AMCO's own file with no record of an original: it did not exist.
		if err := r.e.Files.removeFile(f.Path); err != nil {
			return err
		}
	default:
		klog.Warningf("%s left the config, but its original content was not kept; leaving it in place", f.Path)
		return nil
	}
	r.queueActions(f)
	return nil
}
