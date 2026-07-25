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
func writeFilePreservePerm(path string, data []byte, defaultPerm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	perm := defaultPerm
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	return os.WriteFile(path, data, perm)
}
