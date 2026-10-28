package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// parentDir returns the directory containing path.
func parentDir(path string) string {
	return filepath.Dir(path)
}

// removeFileAndEmptyDir removes path, then removes dir if that leaves it
// empty. dir not being empty (the common case: the manifest shares its
// directory with other rip output) is not an error — os.Remove on a
// non-empty directory fails and that failure is simply ignored.
func removeFileAndEmptyDir(path, dir string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	_ = os.Remove(dir) // best-effort; fails silently when dir still has content
	return nil
}
