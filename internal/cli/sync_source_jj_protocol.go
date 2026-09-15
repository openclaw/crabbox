package cli

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
)

const jjSourceProtocolName = "crabbox-jj-source"
const jjSourceProtocolVersion = 1
const jjSourceNativeVersion = "0.45.1"

type jjProtocolHeader struct {
	Protocol      string `json:"protocol"`
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	JJVersion     string `json:"jj_version"`
}

type jjMaterializationPolicy struct {
	ExecPolicy          string `json:"exec_policy"`
	EOLConversion       string `json:"eol_conversion"`
	ConflictMarkerStyle string `json:"conflict_marker_style"`
	MergeHunkLevel      string `json:"merge_hunk_level"`
	SameChange          string `json:"same_change"`
	MaterializationHost string `json:"materialization_host"`
}

type jjSourceIdentity struct {
	WorkspaceRoot  string                  `json:"workspace_root"`
	RepositoryPath string                  `json:"repository_path"`
	Workspace      string                  `json:"workspace"`
	Operation      string                  `json:"operation"`
	Commit         string                  `json:"commit"`
	Change         string                  `json:"change"`
	Parents        []string                `json:"parents"`
	TreeIDs        []string                `json:"tree_ids"`
	TreeLabels     []string                `json:"tree_labels"`
	Policy         jjMaterializationPolicy `json:"policy"`
}

type jjRecordedTreeTerm struct {
	Kind       string  `json:"kind"`
	ID         string  `json:"id"`
	Executable *bool   `json:"executable,omitempty"`
	CopyID     *string `json:"copy_id,omitempty"`
}

type jjRecordedEntry struct {
	Path         string                `json:"path"`
	Kind         string                `json:"kind"`
	Terms        []*jjRecordedTreeTerm `json:"terms"`
	InputBlobIDs []string              `json:"input_blob_ids"`
	SizeBytes    *uint64               `json:"size_bytes"`
}

type jjRecordedInventory struct {
	jjProtocolHeader
	Identity       jjSourceIdentity  `json:"identity"`
	Entries        []jjRecordedEntry `json:"entries"`
	InputUsedBytes uint64            `json:"input_used_bytes"`
}

type jjRecordedExportRequest struct {
	Protocol      string           `json:"protocol"`
	SchemaVersion int              `json:"schema_version"`
	Identity      jjSourceIdentity `json:"identity"`
	Paths         []string         `json:"paths"`
}

func decodeJJProtocolMessage(data []byte, kind string, output any) error {
	var header jjProtocolHeader
	if err := json.Unmarshal(data, &header); err != nil {
		return fmt.Errorf("decode native source protocol header: %w", err)
	}
	if header.Protocol != jjSourceProtocolName || header.SchemaVersion != jjSourceProtocolVersion || header.Kind != kind || header.JJVersion != jjSourceNativeVersion {
		return fmt.Errorf("unsupported native source protocol: %q version %d kind %q native version %q", header.Protocol, header.SchemaVersion, header.Kind, header.JJVersion)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode native source %s: %w", kind, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("native source %s has trailing data", kind)
	}
	return nil
}

func nativeHexID(id string) bool {
	if id == "" {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func validJJTreeLabels(ids, labels []string) bool {
	// Native ConflictLabels is empty for resolved trees and may also be empty
	// for conflicts. Present labels correspond to every ordered merge term.
	return len(ids) > 0 && len(ids)%2 == 1 &&
		(len(labels) == 0 || (len(ids) > 1 && len(labels) == len(ids)))
}

func validateJJSourceIdentity(identity jjSourceIdentity) error {
	if !filepath.IsAbs(identity.WorkspaceRoot) || !filepath.IsAbs(identity.RepositoryPath) || identity.Workspace == "" || !nativeHexID(identity.Operation) || !nativeHexID(identity.Commit) || !nativeHexID(identity.Change) {
		return fmt.Errorf("native source lacks a complete workspace/revision identity")
	}
	if !validJJTreeLabels(identity.TreeIDs, identity.TreeLabels) {
		return fmt.Errorf("native source tree terms and labels are inconsistent")
	}
	for _, id := range append(append([]string{}, identity.Parents...), identity.TreeIDs...) {
		if !nativeHexID(id) {
			return fmt.Errorf("native source has an invalid parent or tree identity")
		}
	}
	policy := identity.Policy
	member := func(value string, options ...string) bool {
		for _, option := range options {
			if value == option {
				return true
			}
		}
		return false
	}
	if !member(policy.ExecPolicy, "auto", "respect", "ignore") || !member(policy.EOLConversion, "none", "input", "input-output") || !member(policy.ConflictMarkerStyle, "diff", "diff-experimental", "snapshot", "git") || !member(policy.MergeHunkLevel, "line", "word") || !member(policy.SameChange, "accept", "keep") || policy.MaterializationHost == "" {
		return fmt.Errorf("unsupported native materialization policy")
	}
	return nil
}

func decodeJJRecordedInventory(data []byte) (jjRecordedInventory, error) {
	var inventory jjRecordedInventory
	if err := decodeJJProtocolMessage(data, "recorded_inventory", &inventory); err != nil {
		return inventory, err
	}
	if err := validateJJSourceIdentity(inventory.Identity); err != nil {
		return inventory, err
	}
	if inventory.InputUsedBytes != 0 {
		return inventory, fmt.Errorf("native recorded inventory unexpectedly materialized file input")
	}
	seen := map[string]bool{}
	for _, entry := range inventory.Entries {
		if !safeRepoRel(entry.Path) || seen[entry.Path] {
			return inventory, fmt.Errorf("invalid or duplicate native recorded path %q", entry.Path)
		}
		seen[entry.Path] = true
		if len(entry.Terms) == 0 || len(entry.Terms)%2 == 0 {
			return inventory, fmt.Errorf("invalid native tree terms for %q", entry.Path)
		}
		for _, term := range entry.Terms {
			if term == nil {
				continue
			}
			if !nativeHexID(term.ID) {
				return inventory, fmt.Errorf("invalid native object identity for %q", entry.Path)
			}
			switch term.Kind {
			case "file":
				if term.Executable == nil || term.CopyID == nil {
					return inventory, fmt.Errorf("incomplete native file term for %q", entry.Path)
				}
			case "symlink", "tree", "submodule":
				if term.Executable != nil || term.CopyID != nil {
					return inventory, fmt.Errorf("unexpected file metadata for %q", entry.Path)
				}
			default:
				return inventory, fmt.Errorf("unsupported native tree term %q", term.Kind)
			}
		}
		switch entry.Kind {
		case "file", "symlink", "submodule":
			if len(entry.Terms) != 1 || entry.Terms[0] == nil || entry.Terms[0].Kind != entry.Kind {
				return inventory, fmt.Errorf("native entry kind and terms disagree for %q", entry.Path)
			}
		case "file_conflict", "other_conflict":
			if len(entry.Terms) < 3 {
				return inventory, fmt.Errorf("native conflict lacks multiple terms for %q", entry.Path)
			}
			if entry.Kind == "file_conflict" {
				for _, term := range entry.Terms {
					if term != nil && term.Kind != "file" {
						return inventory, fmt.Errorf("native file conflict contains a non-file term for %q", entry.Path)
					}
				}
			}
		default:
			return inventory, fmt.Errorf("unsupported native recorded entry kind %q", entry.Kind)
		}
		for _, id := range entry.InputBlobIDs {
			if !nativeHexID(id) {
				return inventory, fmt.Errorf("invalid native input identity for %q", entry.Path)
			}
		}
	}
	return inventory, nil
}

func encodeJJRecordedExportRequest(inventory jjRecordedInventory, paths []string) ([]byte, error) {
	if err := validateJJSourceIdentity(inventory.Identity); err != nil {
		return nil, err
	}
	entries := make(map[string]jjRecordedEntry, len(inventory.Entries))
	for _, entry := range inventory.Entries {
		entries[entry.Path] = entry
	}
	seen := map[string]bool{}
	for _, path := range paths {
		entry, ok := entries[path]
		if !ok || !safeRepoRel(path) || jjRepositoryMetadataPath(path) || seen[path] || entry.Kind == "submodule" {
			return nil, fmt.Errorf("unsupported or duplicate selected native path %q", path)
		}
		seen[path] = true
	}
	if paths == nil {
		paths = []string{}
	}
	return json.Marshal(jjRecordedExportRequest{Protocol: jjSourceProtocolName, SchemaVersion: jjSourceProtocolVersion, Identity: inventory.Identity, Paths: paths})
}

type jjRecordedExportEntry struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	// Filesystem metadata size; symlink storage size need not equal charged target bytes.
	Bytes    uint64  `json:"bytes"`
	UnixMode *uint32 `json:"unix_mode"`
}

type jjRecordedCheckoutCounts struct {
	AddedFiles   uint64 `json:"added_files"`
	UpdatedFiles uint64 `json:"updated_files"`
	RemovedFiles uint64 `json:"removed_files"`
	SkippedFiles uint64 `json:"skipped_files"`
}

type jjRecordedCheckoutCapabilities struct {
	ExecutableBits *bool `json:"executable_bits"`
	Symlinks       *bool `json:"symlinks"`
}

type jjRecordedExport struct {
	jjProtocolHeader
	Identity         jjSourceIdentity               `json:"identity"`
	Entries          []jjRecordedExportEntry        `json:"entries"`
	CheckoutStats    jjRecordedCheckoutCounts       `json:"checkout_stats"`
	Capabilities     jjRecordedCheckoutCapabilities `json:"capabilities"`
	Output           string                         `json:"output"`
	OwnedState       string                         `json:"owned_state"`
	InputUsedBytes   uint64                         `json:"input_used_bytes"`
	OutputUsedBytes  uint64                         `json:"output_used_bytes"`
	ScratchUsedBytes uint64                         `json:"scratch_used_bytes"`
}

// The caller supplies its canonical owned roots; this verifies the response,
// not the staged file contents or the receiver's representation capabilities.
func decodeJJRecordedExport(data []byte, request jjRecordedExportRequest, outputRoot, stateRoot string) (jjRecordedExport, error) {
	var result jjRecordedExport
	if request.Protocol != jjSourceProtocolName || request.SchemaVersion != jjSourceProtocolVersion {
		return result, fmt.Errorf("unsupported native export request protocol")
	}
	if err := validateJJSourceIdentity(request.Identity); err != nil {
		return result, err
	}
	if err := decodeJJProtocolMessage(data, "recorded_export", &result); err != nil {
		return result, err
	}
	expected, err := json.Marshal(request.Identity)
	if err != nil {
		return result, err
	}
	actual, err := json.Marshal(result.Identity)
	if err != nil {
		return result, err
	}
	if !bytes.Equal(expected, actual) {
		return result, fmt.Errorf("native export identity or policy differs from its request")
	}
	if !filepath.IsAbs(outputRoot) || !filepath.IsAbs(stateRoot) || result.Output != outputRoot || result.OwnedState != stateRoot {
		return result, fmt.Errorf("native export belongs to different owned output roots")
	}
	if result.Capabilities.ExecutableBits == nil || result.Capabilities.Symlinks == nil {
		return result, fmt.Errorf("native export omitted its materialization capabilities")
	}
	stats := result.CheckoutStats
	if stats.SkippedFiles != 0 || stats.UpdatedFiles != 0 || stats.RemovedFiles != 0 || stats.AddedFiles != uint64(len(request.Paths)) {
		return result, fmt.Errorf("native export did not materialize the complete fresh selection")
	}
	selected := make(map[string]bool, len(request.Paths))
	for _, path := range request.Paths {
		if selected[path] || !safeRepoRel(path) || jjRepositoryMetadataPath(path) {
			return result, fmt.Errorf("native export request has invalid or duplicate paths")
		}
		selected[path] = true
	}
	if len(result.Entries) != len(selected) {
		return result, fmt.Errorf("native export entry count differs from its selection")
	}
	var total uint64
	allRegular := true
	for _, entry := range result.Entries {
		if !selected[entry.Path] || (entry.Kind != "file" && entry.Kind != "symlink") {
			return result, fmt.Errorf("unexpected native export entry %q", entry.Path)
		}
		delete(selected, entry.Path)
		allRegular = allRegular && entry.Kind == "file"
		if entry.UnixMode != nil && *entry.UnixMode & ^uint32(0o777) != 0 {
			return result, fmt.Errorf("unsupported native export mode for %q", entry.Path)
		}
		if ^uint64(0)-total < entry.Bytes {
			return result, fmt.Errorf("native export byte accounting overflow")
		}
		total += entry.Bytes
	}
	if len(selected) != 0 || allRegular && total != result.OutputUsedBytes {
		return result, fmt.Errorf("native export payload accounting differs from its entries")
	}
	return result, nil
}
