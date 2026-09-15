//go:build ignore

// Independent standard-library oracle for native Go filesystem size estimates.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if err := inspect(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func inspect() error {
	if len(os.Args) != 3 {
		return fmt.Errorf("usage: fixture-file-info ROOT PATHS_JSON")
	}
	data, err := os.ReadFile(os.Args[2])
	if err != nil {
		return err
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return err
	}
	entries := make([]struct {
		Path  string `json:"path"`
		Bytes int64  `json:"bytes"`
	}, len(paths))
	for i, relative := range paths {
		info, err := os.Lstat(filepath.Join(os.Args[1], filepath.FromSlash(relative)))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("unexpected fixture file kind: %s", relative)
		}
		entries[i].Path, entries[i].Bytes = relative, info.Size()
	}
	return json.NewEncoder(os.Stdout).Encode(entries)
}
