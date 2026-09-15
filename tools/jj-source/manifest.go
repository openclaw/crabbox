// Package jjsource exposes the pinned source identity shared with helper tooling.
package jjsource

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed manifest.json
var manifest []byte

type Identity struct {
	NativeVersion    string
	SourceTreeSHA256 string
}

func ReadIdentity() (Identity, error) {
	var value struct {
		SchemaVersion int `json:"schemaVersion"`
		JJ            struct {
			Version string `json:"version"`
		} `json:"jj"`
		SourceTree struct {
			SHA256 string `json:"sha256"`
		} `json:"sourceTree"`
	}
	if err := json.Unmarshal(manifest, &value); err != nil {
		return Identity{}, err
	}
	if value.SchemaVersion != 1 || value.JJ.Version == "" || len(value.SourceTree.SHA256) != 64 {
		return Identity{}, fmt.Errorf("invalid embedded native source identity")
	}
	return Identity{NativeVersion: value.JJ.Version, SourceTreeSHA256: value.SourceTree.SHA256}, nil
}
