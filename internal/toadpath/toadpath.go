// Package toadpath resolves the toad home directory.
package toadpath

import (
	"os"
	"path/filepath"
)

// Home returns the toad home directory.
// Uses TOAD_HOME env var if set, otherwise ~/.toad.
func Home() (string, error) {
	if v := os.Getenv("TOAD_HOME"); v != "" {
		return filepath.Abs(v)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".toad"), nil
}
