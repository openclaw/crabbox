//go:build darwin && arm64

package utm

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

//go:embed macos_preflight.swift
var macOSPreflightSource string

const preflightEntitlements = `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>com.apple.security.virtualization</key><true/></dict></plist>`

func (b *backend) nativeMacOSPreflight(ctx context.Context, password string) (macOSImage, error) {
	var image macOSImage
	if b.hostOS != "darwin" || b.hostArch != "arm64" {
		return image, fmt.Errorf("fresh macOS requires a local Apple Silicon Mac with macOS 27+, full Xcode and UTM Automation access")
	}
	// A private temporary helper avoids installing or trusting a cached executable.
	dir, err := os.MkdirTemp("", "crabbox-utm-preflight-")
	if err != nil {
		return image, err
	}
	defer os.RemoveAll(dir)
	source, entitlements, helper := filepath.Join(dir, "inspect.swift"), filepath.Join(dir, "entitlements.plist"), filepath.Join(dir, "inspect")
	if err = os.WriteFile(source, []byte(macOSPreflightSource), 0600); err != nil {
		return image, err
	}
	if err = os.WriteFile(entitlements, []byte(preflightEntitlements), 0600); err != nil {
		return image, err
	}
	if _, err = b.command(ctx, "/usr/bin/xcrun", []string{"--sdk", "macosx", "swiftc", "-parse-as-library", source, "-o", helper}, nil); err != nil {
		return image, fmt.Errorf("fresh macOS requires full Xcode with the macOS 27 SDK selected: %w", err)
	}
	if _, err = b.command(ctx, "/usr/bin/codesign", []string{"-s", "-", "--entitlements", entitlements, helper}, nil); err != nil {
		return image, fmt.Errorf("sign native UTM preflight helper: %w", err)
	}
	result, err := b.privateCommand(ctx, "native image/account validation", core.LocalCommandRequest{Name: helper, Args: []string{b.cfg.UTM.IPSW, b.cfg.UTM.User}, Stdin: strings.NewReader(password)})
	if err != nil {
		return image, err
	}
	if err = json.Unmarshal([]byte(result.Stdout), &image); err != nil {
		return macOSImage{}, fmt.Errorf("UTM native preflight returned invalid metadata")
	}
	if err = image.validate(); err != nil {
		return macOSImage{}, err
	}
	return image, nil
}
