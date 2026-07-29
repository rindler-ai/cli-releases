package main

import (
	"os"
	"path/filepath"
)

// readFileOrEmpty reads a file, returning (nil, nil) if it does not exist.
func readFileOrEmpty(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return b, nil
}

// writeFilePreservePerm writes data to path, creating parent dirs (0700). If the
// file already exists its permission bits are preserved; otherwise defaultPerm is
// used. Config files that already exist (e.g. a user's ~/.claude.json) keep their
// own mode rather than being narrowed/widened by us.
// The write is ATOMIC: temp file in the same directory, fsync, then rename over
// the target. These are other tools' config files (~/.claude.json holds every
// project's Claude Code state), so a crash or a full disk partway through a
// truncating write would destroy data we do not own and cannot restore.
func writeFilePreservePerm(path string, data []byte, defaultPerm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	perm := defaultPerm
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, ".rindler-*.tmp")
	if err != nil {
		// Fall back to the direct write rather than refuse to install.
		return os.WriteFile(path, data, perm)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
