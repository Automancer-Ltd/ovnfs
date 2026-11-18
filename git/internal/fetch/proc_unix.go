//go:build !windows

package fetch

import (
	"os"
	"path/filepath"
)

// findExecutable reports dir/name if it is an executable file.
func findExecutable(dir, name string) (string, bool) {
	p := filepath.Join(dir, name)
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return "", false
	}
	return p, true
}
