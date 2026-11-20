// Package atomicfile writes files atomically: unique temp file in the
// target directory, then rename. The unique name is the point — every
// persist site used a fixed "<path>.tmp", and concurrent saves wrote
// the SAME temp file from two goroutines, interleaving bytes before
// one rename published the mixture (sessions.json could come back
// corrupt, and a corrupt sessions.json silently starts the store
// empty — every conversation gone, no error anywhere).
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write atomically replaces path with data at perm. Concurrent calls
// are safe: each gets its own temp file, and the last rename wins —
// a stale-but-complete snapshot at worst, never a torn one.
func Write(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
