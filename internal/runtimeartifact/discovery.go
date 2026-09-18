package runtimeartifact

import (
	"fmt"
	"os"
	"path/filepath"
)

// distribution is set only by the official controller build recipe. A display
// version cannot distinguish a tagged go install from a complete release.
var distribution = "development"

// Discover resolves the real controller and its offline companion manifest.
// An explicit manifest takes precedence. An empty manifest with no error means
// a CLI-only installation; an existing incomplete pack is an error.
func Discover(controllerPath, explicitManifest string) (controller, manifestPath string, err error) {
	return discover(controllerPath, explicitManifest, distribution)
}

func discover(controllerPath, explicitManifest, kind string) (controller, manifestPath string, err error) {
	if kind != "development" && kind != "complete" {
		return "", "", fmt.Errorf("unknown runtime distribution %q; rebuild or reinstall the controller", kind)
	}
	controller, err = filepath.EvalSymlinks(controllerPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve controller executable: %w", err)
	}
	controller, err = filepath.Abs(controller)
	if err != nil {
		return "", "", err
	}
	if explicitManifest != "" {
		manifestPath, err = filepath.Abs(explicitManifest)
		return controller, manifestPath, err
	}
	directory := filepath.Join(filepath.Dir(controller), "crabbox-runtime")
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		if kind == "complete" {
			return controller, "", fmt.Errorf("complete runtime pack is missing; reinstall the matching release")
		}
		return controller, "", nil
	}
	if err != nil {
		return controller, "", fmt.Errorf("inspect runtime pack: %w", err)
	}
	if !info.IsDir() {
		return controller, "", fmt.Errorf("runtime pack must be a directory: %s", directory)
	}
	manifestPath = filepath.Join(directory, "manifest.json")
	info, err = os.Lstat(manifestPath)
	if err != nil {
		return controller, "", fmt.Errorf("incomplete runtime pack; reinstall the matching release: %w", err)
	}
	if !info.Mode().IsRegular() {
		return controller, "", fmt.Errorf("runtime manifest must be a regular file: %s", manifestPath)
	}
	return controller, manifestPath, nil
}
