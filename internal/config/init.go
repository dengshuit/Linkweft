package config

import (
	_ "embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed template.toml
var template string

var errWriteConfig = errors.New("cannot write the configuration file")

// Initialize returns the resolved path and whether it created a new configuration file.
func Initialize(path *string) (string, bool, error) {
	filename, err := configPath(path)
	if err != nil {
		return "", false, err
	}
	// Keep the original parent path so the filesystem resolves symlinks and dot segments.
	dir, base := filepath.Split(filename)
	if base == "" {
		return "", false, errWriteConfig
	}
	if dir == filepath.VolumeName(dir) {
		dir += "."
	}
	info, err := os.Stat(filename)
	if err == nil && info.Mode().IsRegular() {
		return filename, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", false, errWriteConfig
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", false, errors.New("cannot create the configuration directory")
	}
	file, err := os.CreateTemp(dir, ".linkweft-config-*")
	if err != nil {
		return "", false, errWriteConfig
	}
	defer os.Remove(file.Name())
	_, writeErr := file.WriteString(template)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return "", false, errWriteConfig
	}

	// Publish the complete template in one step without replacing an existing file.
	err = os.Link(file.Name(), filename)
	if errors.Is(err, fs.ErrExist) {
		if info, statErr := os.Stat(filename); statErr == nil && info.Mode().IsRegular() {
			return filename, false, nil
		}
	}
	if err != nil {
		return "", false, errWriteConfig
	}
	return filename, true, nil
}
