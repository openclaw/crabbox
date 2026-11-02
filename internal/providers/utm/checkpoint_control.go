package utm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type nativeSnapshot struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	IncludesRunningState *bool  `json:"includesRunningState"`
	DataMissing          *bool  `json:"dataMissing"`
	Modified             string `json:"modified"`
}

// Use dictionary property names rather than the Swift implementation's KVC
// keys. The UUID is the public snapshot ID, not its internal backend identifier.
const snapshotListScript = `function run(argv) {
 var app = Application(argv[0]);
 var vm = app.virtualMachines.whose({id:argv[1]})()[0];
 if (!vm) throw Error("exact UTM VM not found");
 if (argv[2] === "scan") vm.scanSnapshots();
 return JSON.stringify(vm.snapshots().map(function(s) {
  return {id:s.id(),name:s.name(),includesRunningState:s.includesRunningState(),dataMissing:s.dataMissing(),modified:s.modificationDate().toISOString()};
 }));
}`

func (b *backend) snapshots(ctx context.Context, vmID string, scan bool) ([]nativeSnapshot, error) {
	if !validUUID(vmID) {
		return nil, fmt.Errorf("UTM snapshot inventory requires exact VM UUID")
	}
	mode := "list"
	if scan {
		mode = "scan"
	}
	result, err := b.command(ctx, "/usr/bin/osascript", []string{"-l", "JavaScript", "-e", snapshotListScript, b.cfg.UTM.AppPath, vmID, mode}, nil)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(result.Stderr) != "" {
		return nil, fmt.Errorf("UTM snapshot inventory event reported an error; verification unconfirmed: %.1024s", result.Stderr)
	}
	var snapshots []nativeSnapshot
	if err = json.Unmarshal([]byte(result.Stdout), &snapshots); err != nil {
		return nil, fmt.Errorf("decode UTM snapshots: %w", err)
	}
	seen := map[string]bool{}
	for _, s := range snapshots {
		if !validUUID(s.ID) || seen[strings.ToLower(s.ID)] || s.IncludesRunningState == nil || s.DataMissing == nil || s.Modified == "" {
			return nil, fmt.Errorf("UTM returned incomplete or duplicate snapshot identity/state")
		}
		seen[strings.ToLower(s.ID)] = true
	}
	return snapshots, nil
}
func (b *backend) createSnapshot(ctx context.Context, vmID, name string) (string, error) {
	if !validUUID(vmID) {
		return "", fmt.Errorf("UTM snapshot capture requires exact VM UUID")
	}
	script := "tell application " + scriptString(b.cfg.UTM.AppPath) + "\n set targetVM to first virtual machine whose id is " + scriptString(vmID) + "\n if status of targetVM is not stopped then error \"UTM checkpoint capture requires stopped VM\"\n set captured to create snapshot targetVM named " + scriptString(name) + "\n return id of captured\nend tell"
	result, err := b.command(ctx, "/usr/bin/osascript", []string{"-"}, strings.NewReader(script))
	if err == nil && strings.TrimSpace(result.Stderr) != "" {
		err = fmt.Errorf("UTM snapshot create event reported an error; receipt retained: %.1024s", result.Stderr)
	}
	id := strings.TrimSpace(result.Stdout)
	if !validUUID(id) {
		if err == nil {
			err = fmt.Errorf("UTM capture returned no exact snapshot UUID; retain pending checkpoint receipt")
		}
		return "", err
	}
	return id, err
}
func (b *backend) mutateSnapshot(ctx context.Context, action, vmID, snapshotID string) error {
	if !validUUID(vmID) || !validUUID(snapshotID) {
		return fmt.Errorf("UTM snapshot mutation requires exact VM and snapshot UUIDs")
	}
	var command string
	switch action {
	case "restore":
		command = "restore targetSnapshot"
	case "delete":
		command = "delete snapshot targetSnapshot"
	default:
		return fmt.Errorf("unsupported snapshot operation")
	}
	guard := ""
	if action == "restore" {
		guard = " if status of targetVM is not stopped then error \"UTM checkpoint restore requires stopped VM\"\n"
	}
	script := "tell application " + scriptString(b.cfg.UTM.AppPath) + "\n set targetVM to first virtual machine whose id is " + scriptString(vmID) + "\n" + guard + " set targetSnapshot to first snapshot of targetVM whose id is " + scriptString(snapshotID) + "\n " + command + "\nend tell"
	result, err := b.command(ctx, "/usr/bin/osascript", []string{"-"}, strings.NewReader(script))
	if err == nil && strings.TrimSpace(result.Stderr) != "" {
		return fmt.Errorf("UTM snapshot event reported an error; operation unconfirmed: %.1024s", result.Stderr)
	}
	return err
}

func findSnapshot(all []nativeSnapshot, id string) (nativeSnapshot, bool) {
	for _, s := range all {
		if strings.EqualFold(s.ID, id) {
			return s, true
		}
	}
	return nativeSnapshot{}, false
}

type snapshotManifest struct {
	Version       int
	CurrentParent string
	Snapshots     []struct {
		Identifier, BackendIdentifier  string
		HasState, Orphaned, Incomplete bool
	}
}

func (b *backend) snapshotManifest(ctx context.Context, bundle string) (snapshotManifest, error) {
	var manifest snapshotManifest
	path := filepath.Join(bundle, "snapshots.plist")
	info, err := os.Lstat(path)
	if err != nil {
		return manifest, err
	}
	if !info.Mode().IsRegular() {
		return manifest, fmt.Errorf("UTM snapshot manifest must be a regular file inside owned bundle")
	}
	if err = b.readCheckpointPlist(ctx, path, &manifest); err != nil {
		return manifest, err
	}

	if manifest.Version != 1 {
		return manifest, fmt.Errorf("unsupported UTM snapshot manifest version=%d; expected 1", manifest.Version)
	}
	return manifest, nil
}
func (b *backend) validateSnapshotData(ctx context.Context, bundle string, s nativeSnapshot, expectedBackend string) (string, error) {
	if s.DataMissing == nil || *s.DataMissing {
		return "", fmt.Errorf("UTM snapshot %s has missing data; delete it and create a new checkpoint", s.ID)
	}
	manifest, err := b.snapshotManifest(ctx, bundle)
	if err != nil {
		return "", err
	}
	for _, entry := range manifest.Snapshots {
		if strings.EqualFold(entry.Identifier, s.ID) {
			if entry.Orphaned || entry.Incomplete || entry.BackendIdentifier == "" || s.IncludesRunningState == nil || entry.HasState != *s.IncludesRunningState {
				return "", fmt.Errorf("UTM snapshot %s data is missing, incomplete or inconsistent; restore refused", s.ID)
			}
			if expectedBackend != "" && entry.BackendIdentifier != expectedBackend {
				return "", fmt.Errorf("UTM snapshot backend state was overwritten; restore refused")
			}
			return entry.BackendIdentifier, nil
		}
	}
	return "", fmt.Errorf("UTM snapshot UUID missing from owned bundle manifest")
}

func (b *backend) checkpointHost(ctx context.Context, backend string) (string, string, error) {
	var version string
	if err := b.plistExtract(ctx, filepath.Join(b.cfg.UTM.AppPath, "Contents", "Info.plist"), "CFBundleShortVersionString", "raw", &version); err != nil {
		return "", "", err
	}
	parts := strings.Split(version, ".")
	major, _ := strconv.Atoi(parts[0])
	minor := 0
	patch := 0
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	if len(parts) > 2 {
		patch, _ = strconv.Atoi(parts[2])
	}
	if major < 5 || major == 5 && (minor < 0 || minor == 0 && patch < 8) {
		return "", "", fmt.Errorf("UTM native checkpoints require UTM 5.0.8 or later (installed=%s)", version)
	}
	host, err := b.command(ctx, "/usr/bin/sw_vers", []string{"-productVersion"}, nil)
	if err != nil {
		return "", "", err
	}
	hostVersion := strings.TrimSpace(host.Stdout)
	hostMajor, _ := strconv.Atoi(strings.Split(hostVersion, ".")[0])
	if backend == "apple" && hostMajor < 27 {
		return "", "", fmt.Errorf("UTM Apple snapshots require macOS 27 or later (host=%s)", hostVersion)
	}
	if backend != "qemu" && backend != "apple" {
		return "", "", fmt.Errorf("UTM backend=%s does not support checkpoints", backend)
	}
	return version, hostVersion, nil
}
