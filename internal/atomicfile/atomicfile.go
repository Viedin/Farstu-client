// Package atomicfile writes state files whole or not at all.
package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
)

// Write replaces path with b, readable only by its owner, atomically and
// durably: a crash leaves the old file or the new one, never half of one.
func Write(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*") // 0600
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path)) // make the rename itself durable
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
