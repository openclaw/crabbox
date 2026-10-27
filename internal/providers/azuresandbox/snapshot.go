package azuresandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// The infrastructure owner supplies the capture binding and the existing image
// verifier receipt. Its digest is trusted configuration, not remote authority.
type snapshotBinding struct {
	Schema          string            `json:"schema"`
	ProviderScope   string            `json:"providerScope"`
	SnapshotID      string            `json:"snapshotId"`
	SourceSandboxID string            `json:"sourceSandboxId"`
	SourceDiskID    string            `json:"sourceDiskId"`
	CapturedAt      string            `json:"capturedAt"`
	ValidUntil      string            `json:"validUntil"`
	VerifierFile    string            `json:"verifierFile"`
	VerifierSHA256  string            `json:"verifierSha256"`
	Runtime         preparedRuntime   `json:"preparedRuntime"`
	Lifecycle       snapshotLifecycle `json:"lifecycle"`
}

type preparedRuntime struct {
	RuntimeImage        string   `json:"runtimeImage"`
	BuildID             string   `json:"buildId"`
	RuntimeVersion      string   `json:"runtimeVersion"`
	EnabledPluginIDs    []string `json:"enabledPluginIds"`
	NodeBootstrapSHA256 string   `json:"nodeBootstrapSha256"`
	WorkerBundleSHA256  string   `json:"workerBundleSha256"`
	BundleHash          string   `json:"bundleHash"`
	CacheHitVerified    bool     `json:"cacheHitVerified"`
}

type snapshotLifecycle struct {
	AutoSuspend struct {
		Enabled  bool   `json:"enabled"`
		Interval int    `json:"interval"`
		Mode     string `json:"mode"`
	} `json:"autoSuspendPolicy"`
	AutoDelete struct {
		Enabled  bool `json:"enabled"`
		Interval int  `json:"deleteIntervalInSeconds"`
	} `json:"autoDeletePolicy"`
}

type snapshotMetadata struct {
	ID        string `json:"id"`
	SandboxID string `json:"sandboxId"`
	CreatedAt string `json:"createdAtUtc"`
}

// Match the documented SDK read aliases; writes use the canonical Policy keys.
func (p *snapshotLifecycle) UnmarshalJSON(data []byte) error {
	type canonical snapshotLifecycle
	var wire struct {
		canonical
		AutoSuspend json.RawMessage `json:"autoSuspend"`
		AutoDelete  json.RawMessage `json:"autoDelete"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*p = snapshotLifecycle(wire.canonical)
	if len(wire.AutoSuspend) != 0 {
		if err := json.Unmarshal(wire.AutoSuspend, &p.AutoSuspend); err != nil {
			return err
		}
	}
	if len(wire.AutoDelete) != 0 {
		if err := json.Unmarshal(wire.AutoDelete, &p.AutoDelete); err != nil {
			return err
		}
	}
	return nil
}

func digestMatches(data []byte, expected string) bool {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == expected
}

func readPinnedFile(path, digest string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ACA snapshot receipt is unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("ACA snapshot receipt is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || !digestMatches(data, digest) {
		return nil, fmt.Errorf("ACA snapshot receipt does not match its generation pin")
	}
	return data, nil
}

func (b *backend) snapshotBinding() (*snapshotBinding, error) {
	cfg := b.cfg.AzureSandbox
	if cfg.SnapshotID == "" {
		if cfg.SnapshotReceipt != "" || cfg.SnapshotGeneration != "" {
			return nil, fmt.Errorf("ACA snapshot receipt requires a snapshot ID")
		}
		return nil, nil
	}
	if cfg.DiskID == "" || cfg.SnapshotReceipt == "" || cfg.SnapshotGeneration == "" {
		return nil, fmt.Errorf("ACA snapshot requires a source disk and pinned clean receipt")
	}
	data, err := readPinnedFile(cfg.SnapshotReceipt, cfg.SnapshotGeneration)
	if err != nil {
		return nil, err
	}
	var binding snapshotBinding
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&binding) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("ACA snapshot binding is invalid")
	}
	validUntil, expiryErr := time.Parse(time.RFC3339Nano, binding.ValidUntil)
	capturedAt, captureErr := time.Parse(time.RFC3339Nano, binding.CapturedAt)
	if binding.Schema != "crabbox.azure-sandbox-snapshot.v1" || binding.ProviderScope != (Provider{}).ClaimScope(b.cfg) ||
		binding.SnapshotID != cfg.SnapshotID || binding.SourceDiskID != cfg.DiskID || binding.SourceSandboxID == "" ||
		expiryErr != nil || captureErr != nil || capturedAt.After(time.Now()) || !validUntil.After(capturedAt) || !validUntil.After(time.Now()) {
		return nil, fmt.Errorf("ACA snapshot binding is stale or differs from the selected source")
	}
	if !binding.Lifecycle.AutoSuspend.Enabled || binding.Lifecycle.AutoSuspend.Mode != "Disk" ||
		binding.Lifecycle.AutoSuspend.Interval != int(b.cfg.IdleTimeout.Seconds()) || !binding.Lifecycle.AutoDelete.Enabled ||
		binding.Lifecycle.AutoDelete.Interval != int(b.cfg.TTL.Seconds()) {
		return nil, fmt.Errorf("ACA snapshot lifecycle differs from the requested lease policy")
	}
	file := binding.VerifierFile
	if !filepath.IsAbs(file) {
		file = filepath.Join(filepath.Dir(cfg.SnapshotReceipt), file)
	}
	verified, err := readPinnedFile(file, binding.VerifierSHA256)
	if err != nil {
		return nil, err
	}
	// Extra tool/repository inventory belongs to the existing verifier; consume
	// only its attestation and exact runtime identity, never paths or secrets.
	var receipt struct {
		PreEnrollment struct {
			Schema                             string `json:"schema"`
			CredentialEnvironmentAbsent        bool   `json:"credentialEnvironmentAbsent"`
			CredentialAndSessionPathsAbsent    bool   `json:"credentialAndSessionPathsAbsent"`
			WorkerStateLimitedToPreparedCaches bool   `json:"workerStateLimitedToPreparedCaches"`
			WorkspaceEmpty                     bool   `json:"workspaceEmpty"`
		} `json:"preEnrollment"`
		CheckedCredentialPathsAbsent bool            `json:"checkedCredentialPathsAbsent"`
		Runtime                      preparedRuntime `json:"preparedRuntime"`
	}
	if json.Unmarshal(verified, &receipt) != nil {
		return nil, fmt.Errorf("ACA snapshot verifier receipt is invalid")
	}
	p := receipt.PreEnrollment
	if p.Schema != "crabbox.pre-enrollment-state.v1" || !p.CredentialEnvironmentAbsent || !p.CredentialAndSessionPathsAbsent || !p.WorkerStateLimitedToPreparedCaches ||
		!p.WorkspaceEmpty || !receipt.CheckedCredentialPathsAbsent || !receipt.Runtime.CacheHitVerified || !reflect.DeepEqual(receipt.Runtime, binding.Runtime) {
		return nil, fmt.Errorf("ACA snapshot lacks matching clean pre-enrollment runtime attestation")
	}
	r := binding.Runtime
	_, imageDigest, immutableImage := strings.Cut(r.RuntimeImage, "@sha256:")
	if r.BuildID == "" || !immutableImage || r.RuntimeVersion == "" || r.EnabledPluginIDs == nil {
		return nil, fmt.Errorf("ACA snapshot runtime generation is incomplete")
	}
	for _, digest := range []string{imageDigest, r.NodeBootstrapSHA256, r.WorkerBundleSHA256, r.BundleHash} {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size {
			return nil, fmt.Errorf("ACA snapshot runtime digest is invalid")
		}
	}
	return &binding, nil
}

func (b *backend) verifySnapshotSource(ctx context.Context, binding *snapshotBinding) error {
	if binding == nil {
		return nil
	}
	for _, id := range []string{binding.SnapshotID, binding.SourceDiskID, binding.SourceSandboxID} {
		if !resourceSegment.MatchString(id) || id == "." || id == ".." {
			return fmt.Errorf("ACA snapshot source identifier is invalid")
		}
	}
	var snapshot snapshotMetadata
	if err := b.api.json(ctx, "snapshot-inspect", "GET", b.api.groupPath+"/snapshots/"+binding.SnapshotID, nil, &snapshot); err != nil {
		return err
	}
	if snapshot.ID != binding.SnapshotID || snapshot.SandboxID != binding.SourceSandboxID || snapshot.CreatedAt != binding.CapturedAt {
		return fmt.Errorf("ACA snapshot metadata differs from its captured source")
	}
	var disk struct {
		ID     string `json:"id"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := b.api.json(ctx, "snapshot-disk-inspect", "GET", b.api.groupPath+"/diskimages/"+binding.SourceDiskID, nil, &disk); err != nil {
		return err
	}
	if disk.ID != binding.SourceDiskID || disk.Status.State != "Ready" {
		return fmt.Errorf("ACA snapshot source disk is not ready")
	}
	return nil
}

// Snapshot restore does not inherit the source's lifecycle. Configure only the
// fresh owned clone. An uncertain setter is reconciled by GET, never re-posted.
func (b *backend) configureSnapshotLifecycle(ctx context.Context, tx *core.FixedTransaction, s sandbox, policy snapshotLifecycle) error {
	if s.Lifecycle == policy {
		return nil
	}
	if tx.Claim.FixedCreateIntent.Attempt["snapshot_policy_submitted"] != "" {
		return fmt.Errorf("ACA snapshot lifecycle update remains uncertain; reconcile or release this lease")
	}
	if err := tx.Bind(core.FixedResourceBinding{AttemptValues: map[string]string{"snapshot_policy_submitted": "true"}}); err != nil {
		return err
	}
	if err := tx.Record("bound"); err != nil {
		return err
	}
	path, err := b.api.sandboxPath(s.ID)
	if err != nil {
		return err
	}
	if err := b.api.json(ctx, "snapshot-lifecycle", "POST", path+"/lifecycle", policy, nil); err != nil {
		return err
	}
	observed, err := b.inspect(ctx, *tx.Claim)
	if err != nil {
		return err
	}
	if observed.Lifecycle != policy {
		return fmt.Errorf("ACA snapshot lifecycle update lacks matching readback; retain and reconcile this lease")
	}
	return nil
}
