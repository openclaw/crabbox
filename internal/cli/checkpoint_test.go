package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateCheckpointID(t *testing.T) {
	if got, err := validateCheckpointID("chk_abc-123_DEF"); err != nil || got != "chk_abc-123_DEF" {
		t.Fatalf("valid id got=%q err=%v", got, err)
	}
	for _, id := range []string{"", "abc", "chk_", "../chk_bad", "chk_bad/slash", "chk_bad space"} {
		t.Run(id, func(t *testing.T) {
			if _, err := validateCheckpointID(id); err == nil {
				t.Fatalf("expected %q to fail", id)
			}
		})
	}
}

func TestCheckpointRecordRoundTripAndListOrder(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	first := checkpointRecord{
		ID:        "chk_first",
		Kind:      checkpointKindArchive,
		CreatedAt: "2026-05-13T10:00:00Z",
		Workdir:   "/work/cbx_1/my-app",
	}
	first.Repo.Name = "my-app"
	second := first
	second.ID = "chk_second"
	second.CreatedAt = "2026-05-13T11:00:00Z"
	if _, err := store.Create(first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(second); err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].ID != "chk_second" || records[1].ID != "chk_first" {
		t.Fatalf("unexpected order: %#v", records)
	}
	got, _, err := store.Read("chk_first")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workdir != first.Workdir || got.Repo.Name != "my-app" {
		t.Fatalf("round trip got=%#v", got)
	}
}

func TestCleanupUncommittedCheckpointDirOnCreateError(t *testing.T) {
	dir := t.TempDir()
	cleanupUncommittedCheckpointDir(dir, false, io.ErrUnexpectedEOF)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("partial checkpoint dir still exists: err=%v", err)
	}

	committedDir := t.TempDir()
	cleanupUncommittedCheckpointDir(committedDir, true, io.ErrUnexpectedEOF)
	if _, err := os.Stat(committedDir); err != nil {
		t.Fatalf("committed checkpoint dir removed: %v", err)
	}
}

func TestCreateCheckpointArchiveCleansCreatedDirOnFailure(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "chk_partial")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := createCheckpointArchive(ctx, SSHTarget{User: "nobody", Host: "127.0.0.1", Port: "1", TargetOS: targetLinux}, "/work/missing", filepath.Join(dir, checkpointArchive))
	if err == nil {
		t.Fatal("expected archive failure")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("partial archive dir still exists: err=%v", err)
	}
}

func TestCheckpointDeleteReturnsMetadataReadError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.checkpointDelete(context.Background(), []string{"chk_missing"}); err == nil {
		t.Fatal("expected missing checkpoint delete to fail")
	}
	if stdout.String() != "" {
		t.Fatalf("stdout=%q, want empty", stdout.String())
	}
}

func TestCheckpointDeleteKeepsCorruptRecord(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir, err := checkpointDir("chk_corrupt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, checkpointMetaFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	if err := app.checkpointDelete(context.Background(), []string{"chk_corrupt"}); err == nil {
		t.Fatal("expected corrupt checkpoint delete to fail")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("corrupt checkpoint dir removed: %v", err)
	}
}

func TestCheckpointDeleteDryRunKeepsRecordedCheckpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(checkpointRecord{ID: "chk_delete_dryrun", Kind: checkpointKindArchive, CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.checkpointDelete(context.Background(), []string{record.ID, "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "would delete checkpoint") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if _, _, err := store.Read(record.ID); err != nil {
		t.Fatalf("dry-run deleted checkpoint: %v", err)
	}
}

func TestCheckpointRestoreDryRunDoesNotResolveLease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(checkpointRecord{ID: "chk_restore_dryrun", Kind: checkpointKindArchive, CreatedAt: time.Now().UTC().Format(time.RFC3339), Workdir: "/work/cbx_old/my-app"})
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.checkpointRestore(context.Background(), []string{record.ID, "--id", "cbx_missing", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "would restore checkpoint") || !strings.Contains(stdout.String(), "cbx_missing") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

// TestCheckpointRestoreDockerCommitDoesNotPointAtFork is the round-10 regression:
// restoring a docker-commit checkpoint must not tell users to use `checkpoint
// fork` (which lands separately) or call the image a "VM image"; it should point
// at the create/verify/delete support this PR adds.
func TestCheckpointRestoreDockerCommitDoesNotPointAtFork(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record := checkpointRecord{ID: "chk_dc_restore", Kind: checkpointKindDockerCommit, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	record.Native.ImageID = "sha256:deadbeef"
	if _, err := store.Create(record); err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err = app.checkpointRestore(context.Background(), []string{record.ID, "--id", "cbx_x"})
	if err == nil {
		t.Fatal("expected restore of a docker-commit checkpoint to be unsupported")
	}
	msg := err.Error()
	if strings.Contains(msg, "fork") {
		t.Fatalf("docker-commit restore guidance must not point at fork, got %q", msg)
	}
	if strings.Contains(msg, "VM image") {
		t.Fatalf("docker-commit image must not be called a VM image, got %q", msg)
	}
	// Must point at commands that actually exist: `inspect <id> --verify` and
	// `delete <id>` (there is no `checkpoint verify` subcommand).
	if !strings.Contains(msg, "checkpoint inspect") || !strings.Contains(msg, "--verify") {
		t.Fatalf("guidance should point at `checkpoint inspect <id> --verify`, got %q", msg)
	}
	if !strings.Contains(msg, "checkpoint delete") {
		t.Fatalf("guidance should point at `checkpoint delete <id>`, got %q", msg)
	}
}

func TestCheckpointForkDryRunDoesNotAcquireLease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(checkpointRecord{ID: "chk_fork_dryrun", Kind: checkpointKindArchive, CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.checkpointFork(context.Background(), []string{record.ID, "--dry-run", "--slug", "fork-dryrun"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "would fork checkpoint") || !strings.Contains(stdout.String(), "fork-dryrun") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestCheckpointDeleteParallelsSnapshotRejectsLocalOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.checkpointDelete(context.Background(), []string{
		"--provider", "parallels",
		"--id", "Ubuntu 25.10",
		"--snapshot", "fresh",
		"--local-only",
	})
	if err == nil || !strings.Contains(err.Error(), "--local-only applies only to recorded checkpoints") {
		t.Fatalf("err=%v", err)
	}
}

func TestParallelsSnapshotCheckpointViewMarksForkablePoweroffOnly(t *testing.T) {
	poweredOff := parallelsSnapshotCheckpointView("vm1", ParallelsSnapshot{
		ID:      "{snap1}",
		Name:    "known-good",
		Date:    "2026-03-12 13:55:00",
		State:   "poweroff",
		Current: true,
		Parent:  "{parent}",
	})
	if !poweredOff.Forkable || poweredOff.Reason != "" || poweredOff.Source != "vm1" {
		t.Fatalf("poweredOff=%#v", poweredOff)
	}

	poweredOn := parallelsSnapshotCheckpointView("vm1", ParallelsSnapshot{ID: "{snap2}", Name: "live", State: "poweron"})
	if poweredOn.Forkable || !strings.Contains(poweredOn.Reason, "power-off") {
		t.Fatalf("poweredOn=%#v", poweredOn)
	}
}

func TestDirectParallelsCheckpointRefusesRunningVMWithNoReboot(t *testing.T) {
	runner := &checkpointParallelsRunner{vmState: "running", snapshotState: "poweroff"}
	_, err := (directParallelsCheckpointDriver{Runner: runner}).Create(context.Background(), checkpointNativeCreateRequest{
		Cfg:      Config{Provider: "parallels"},
		Server:   Server{CloudID: "vm1", Labels: map[string]string{}},
		LeaseID:  "cbx_123",
		RepoName: "my-app",
		NoReboot: true,
	})
	if err == nil || !strings.Contains(err.Error(), "require a powered-off VM") {
		t.Fatalf("err=%v", err)
	}
	if runner.called("stop") || runner.called("snapshot") {
		t.Fatalf("unexpected mutating command: %#v", runner.commands)
	}
}

func TestDirectParallelsCheckpointStopsAndRestartsForForkableSnapshot(t *testing.T) {
	runner := &checkpointParallelsRunner{vmState: "running", snapshotState: "poweroff"}
	image, err := (directParallelsCheckpointDriver{Runner: runner}).Create(context.Background(), checkpointNativeCreateRequest{
		Cfg:      Config{Provider: "parallels"},
		Server:   Server{CloudID: "vm1", Labels: map[string]string{}},
		LeaseID:  "cbx_123",
		RepoName: "my-app",
		NoReboot: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if image.ID != "{snap1}" || image.State != "poweroff" {
		t.Fatalf("image=%#v", image)
	}
	for _, want := range []string{"stop", "snapshot", "snapshot-list", "start"} {
		if !runner.called(want) {
			t.Fatalf("missing %s command: %#v", want, runner.commands)
		}
	}
}

func TestParallelsSnapshotCheckpointViewsTreeAndFilters(t *testing.T) {
	snapshots := []ParallelsSnapshot{
		{ID: "{child}", Name: "child", Parent: "{root}", Date: "2026-01-02", State: "poweroff"},
		{ID: "{root}", Name: "root", Date: "2026-01-01", State: "poweron"},
		{ID: "{sibling}", Name: "sibling", Parent: "{root}", Date: "2026-01-03", State: "poweron", Current: true},
	}
	views := parallelsSnapshotCheckpointViews("vm1", snapshots, checkpointParallelsListOptions{Tree: true})
	if len(views) != 3 || views[0].Name != "root" || views[1].Name != "child" || views[1].Depth != 1 {
		t.Fatalf("views=%#v", views)
	}
	views = parallelsSnapshotCheckpointViews("vm1", snapshots, checkpointParallelsListOptions{Tree: true, ForkableOnly: true})
	if len(views) != 1 || views[0].Name != "child" {
		t.Fatalf("forkable views=%#v", views)
	}
	views = parallelsSnapshotCheckpointViews("vm1", snapshots, checkpointParallelsListOptions{Tree: true, CurrentOnly: true})
	if len(views) != 1 || views[0].Name != "sibling" {
		t.Fatalf("current views=%#v", views)
	}
}

func TestApplyParallelsCheckpointHostConfigPreservesFleetHostAuth(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "ssh"
	cfg.Parallels.Hosts = []ParallelsHostConfig{{
		Name:   "mac-fleet-1",
		Host:   "mac-host.example.net",
		User:   "builder",
		Key:    "~/.ssh/mac-host",
		VMRoot: "/Users/builder/Parallels",
	}}
	record := checkpointRecord{Kind: checkpointKindParallels}
	record.Native.Region = "mac-host.example.net"

	applyParallelsCheckpointHostConfig(&cfg, record)
	if cfg.Provider != "parallels" || cfg.Parallels.Host != "mac-host.example.net" || cfg.Parallels.HostUser != "builder" || cfg.Parallels.HostKey != "~/.ssh/mac-host" || cfg.Parallels.VMRoot != "/Users/builder/Parallels" || cfg.Parallels.SelectedHost != "mac-fleet-1" {
		t.Fatalf("cfg=%#v", cfg.Parallels)
	}
	if got := parallelsHostRefForConfig(cfg); got != "mac-fleet-1" {
		t.Fatalf("host ref=%q", got)
	}
}

func TestCheckpointInspectVerifyArchiveStates(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(checkpointRecord{
		ID:          "chk_archive",
		Kind:        checkpointKindArchive,
		CreatedAt:   "2026-05-13T10:00:00Z",
		ArchivePath: checkpointArchive,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := store.Paths(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.checkpointInspect(context.Background(), []string{record.ID, "--verify", "--json"}); err != nil {
		t.Fatal(err)
	}
	var audit checkpointAudit
	if err := json.Unmarshal(stdout.Bytes(), &audit); err != nil {
		t.Fatal(err)
	}
	if audit.LocalState != "available" || audit.ProviderState != "not_applicable" || audit.NextAction != "restore_or_fork" {
		t.Fatalf("audit=%#v", audit)
	}

	if err := os.Remove(paths.Archive); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := app.checkpointInspect(context.Background(), []string{record.ID, "--verify", "--json"}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(stdout.Bytes(), &audit); err != nil {
		t.Fatal(err)
	}
	if audit.LocalState != "missing_archive" || audit.NextAction != "delete_or_recreate" {
		t.Fatalf("missing archive audit=%#v", audit)
	}
}

func TestCheckpointPruneDryRunAndDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	oldRecord, err := store.Create(checkpointRecord{
		ID:        "chk_old",
		Kind:      checkpointKindArchive,
		CreatedAt: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(checkpointRecord{
		ID:        "chk_new",
		Kind:      checkpointKindArchive,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.checkpointPrune(context.Background(), []string{"--older-than", "24h", "--kind", "archive", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "would delete id=chk_old") {
		t.Fatalf("dry-run stdout=%q", stdout.String())
	}
	if _, _, err := store.Read(oldRecord.ID); err != nil {
		t.Fatalf("dry-run deleted checkpoint: %v", err)
	}

	stdout.Reset()
	if err := app.checkpointPrune(context.Background(), []string{"--older-than", "24h", "--kind", "archive"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "checkpoint pruned id=chk_old") {
		t.Fatalf("prune stdout=%q", stdout.String())
	}
	if _, _, err := store.Read(oldRecord.ID); err == nil {
		t.Fatal("old checkpoint still exists")
	}
	if _, _, err := store.Read("chk_new"); err != nil {
		t.Fatalf("new checkpoint removed: %v", err)
	}
}

func TestCheckpointPruneRejectsOperands(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(checkpointRecord{
		ID:        "chk_old",
		Kind:      checkpointKindArchive,
		CreatedAt: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	app := App{Stdout: io.Discard, Stderr: io.Discard}
	if err := app.checkpointPrune(context.Background(), []string{"chk_old", "--older-than", "24h"}); err == nil {
		t.Fatal("expected unexpected operand error")
	}
	if _, _, err := store.Read("chk_old"); err != nil {
		t.Fatalf("checkpoint removed after invalid prune command: %v", err)
	}
}

func TestNewCheckpointRecordUsesResolvedVersion(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldVersion := version
	version = "v1.2.3"
	t.Cleanup(func() { version = oldVersion })

	record, _, err := newCheckpointRecord(
		Repo{Name: "my-app"},
		defaultConfig(),
		Server{CloudID: "i-1234567890abcdef0", Provider: "aws"},
		SSHTarget{TargetOS: targetLinux},
		"cbx_123",
		"/work/cbx_123/my-app",
		"test checkpoint",
	)
	if err != nil {
		t.Fatal(err)
	}
	if record.CrabboxVersion != "1.2.3" {
		t.Fatalf("CrabboxVersion=%q, want 1.2.3", record.CrabboxVersion)
	}
}

func TestNewCheckpointRecordStoresHostPinAndServerType(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetMacOS
	cfg.HostID = "h-000000000001"
	cfg.ServerType = "mac2-m2pro.metal"

	record, _, err := newCheckpointRecord(
		Repo{Name: "my-app"},
		cfg,
		Server{CloudID: "i-1234567890abcdef0", Provider: "aws", HostID: "h-000000000002"},
		SSHTarget{TargetOS: targetMacOS},
		"cbx_123",
		"/Users/ec2-user/crabbox/cbx_123/my-app",
		"test checkpoint",
	)
	if err != nil {
		t.Fatal(err)
	}
	if record.HostID != "h-000000000002" {
		t.Fatalf("HostID=%q, want h-000000000002", record.HostID)
	}
	if record.ServerType != "mac2-m2pro.metal" {
		t.Fatalf("ServerType=%q, want mac2-m2pro.metal", record.ServerType)
	}
}

func TestDefaultCheckpointRestoreWorkdirUsesTargetLease(t *testing.T) {
	cfg := defaultConfig()
	cfg.WorkRoot = "/work"
	got := defaultCheckpointRestoreWorkdir(cfg, "cbx_new", "my-app", "/work/cbx_old/my-app")
	if got != "/work/cbx_new/my-app" {
		t.Fatalf("restore workdir = %q, want target lease workdir", got)
	}
}

func TestCheckpointCreateModePrefersDiskSnapshotLinuxNative(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "hetzner"
	cfg.Coordinator = "https://coordinator.example"
	cfg.TargetOS = targetLinux
	server := Server{Provider: "aws", CloudID: "i-123"}
	target := SSHTarget{TargetOS: targetLinux}
	if got := checkpointCreateMode("auto", "", cfg, server, target, false); got != checkpointKindAWSEBS {
		t.Fatalf("mode=%q", got)
	}
	if got := checkpointCreateMode("native", "image", cfg, server, target, false); got != checkpointKindAWSAMI {
		t.Fatalf("image strategy mode=%q", got)
	}
	if got := checkpointCreateMode("image", "", cfg, server, target, false); got != checkpointKindAWSAMI || checkpointStrategyForKind(got) != checkpointStrategyImage {
		t.Fatalf("legacy image mode=%q strategy=%q", got, checkpointStrategyForKind(got))
	}
}

func TestCheckpointCreateModeSupportsAWSMacOSAMIBackedCheckpoints(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Coordinator = "https://coordinator.example"
	cfg.TargetOS = targetMacOS
	cfg.Capacity.Market = "on-demand"
	server := Server{Provider: "aws", CloudID: "i-123"}
	target := SSHTarget{TargetOS: targetMacOS}

	if got := checkpointCreateMode("auto", "", cfg, server, target, false); got != checkpointKindAWSAMI {
		t.Fatalf("mode=%q, want %q", got, checkpointKindAWSAMI)
	}
	if got := checkpointCreateMode("native", "image", cfg, server, target, false); got != checkpointKindAWSAMI {
		t.Fatalf("image strategy mode=%q, want %q", got, checkpointKindAWSAMI)
	}
	if got := checkpointCreateMode("snapshot", "", cfg, server, target, false); got != checkpointKindAWSAMI {
		t.Fatalf("snapshot mode=%q, want %q", got, checkpointKindAWSAMI)
	}
}

func TestCheckpointCreateModeSupportsAzureAndGCPNative(t *testing.T) {
	cfg := defaultConfig()
	cfg.Coordinator = "https://coordinator.example"
	cfg.TargetOS = targetLinux
	target := SSHTarget{TargetOS: targetLinux}
	for _, tc := range []struct {
		provider string
		want     string
	}{
		{provider: "azure", want: checkpointKindAzureOS},
		{provider: "gcp", want: checkpointKindGCPDisk},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			server := Server{Provider: tc.provider, CloudID: "vm-123"}
			if got := checkpointCreateMode("auto", "", cfg, server, target, false); got != tc.want {
				t.Fatalf("mode=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckpointCreateModeAutoFallsBackForDirectAWS(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Coordinator = ""
	cfg.TargetOS = targetLinux
	server := Server{Provider: "aws", CloudID: "i-123"}
	target := SSHTarget{TargetOS: targetLinux}
	if got := checkpointCreateMode("auto", "", cfg, server, target, false); got != checkpointKindArchive {
		t.Fatalf("mode=%q, want archive", got)
	}
}

func TestCheckpointCreateModeNativeSupportsDirectAWSAMI(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Coordinator = ""
	cfg.TargetOS = targetMacOS
	server := Server{Provider: "aws", CloudID: "i-123"}
	target := SSHTarget{TargetOS: targetMacOS}

	if got := checkpointCreateMode("native", "", cfg, server, target, false); got != checkpointKindAWSAMI {
		t.Fatalf("native mode=%q, want %q", got, checkpointKindAWSAMI)
	}
	if got := checkpointCreateMode("native", "image", cfg, server, target, false); got != checkpointKindAWSAMI {
		t.Fatalf("native image mode=%q, want %q", got, checkpointKindAWSAMI)
	}
	if got := checkpointCreateMode("auto", "image", cfg, server, target, false); got != checkpointKindAWSAMI {
		t.Fatalf("auto image mode=%q, want %q", got, checkpointKindAWSAMI)
	}
	if got := checkpointCreateMode("image", "", cfg, server, target, false); got != checkpointKindAWSAMI {
		t.Fatalf("image mode=%q, want %q", got, checkpointKindAWSAMI)
	}
	if got := checkpointCreateMode("snapshot", "", cfg, server, target, false); got != checkpointKindAWSAMI {
		t.Fatalf("snapshot mode=%q, want %q", got, checkpointKindAWSAMI)
	}
}

func TestCheckpointCreateModeParallelsRejectsImageStrategy(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "parallels"
	server := Server{Provider: "parallels", CloudID: "vm-123"}
	target := SSHTarget{TargetOS: targetMacOS}
	if got := checkpointCreateMode("native", checkpointStrategyImage, cfg, server, target, false); got != "unsupported" {
		t.Fatalf("native image mode=%q, want unsupported", got)
	}
	if got := checkpointCreateMode("native", checkpointStrategyDiskSnapshot, cfg, server, target, false); got != checkpointKindParallels {
		t.Fatalf("native disk snapshot mode=%q, want %q", got, checkpointKindParallels)
	}
}

func TestCheckpointCreateModeLocalContainerNativeUsesDockerCommit(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "local-container"
	server := Server{Provider: "local-container", CloudID: "abc123"}
	target := SSHTarget{TargetOS: targetLinux}
	// auto keeps the existing workspace-archive default; docker-commit is opt-in via --mode native.
	if got := checkpointCreateMode("auto", "", cfg, server, target, false); got != checkpointKindArchive {
		t.Fatalf("auto mode=%q, want %q", got, checkpointKindArchive)
	}
	if got := checkpointCreateMode("native", "", cfg, server, target, false); got != checkpointKindDockerCommit {
		t.Fatalf("native mode=%q, want %q", got, checkpointKindDockerCommit)
	}
	for _, strategy := range []string{checkpointStrategyImage, checkpointStrategyDiskSnapshot, "disk", "snapshot"} {
		if got := checkpointCreateMode("native", strategy, cfg, server, target, false); got != "unsupported" {
			t.Fatalf("native strategy=%q mode=%q, want unsupported", strategy, got)
		}
	}
}

func TestCheckpointDockerCommitUsesImageStrategy(t *testing.T) {
	if got := checkpointStrategyForKind(checkpointKindDockerCommit); got != checkpointStrategyImage {
		t.Fatalf("strategy=%q, want %q", got, checkpointStrategyImage)
	}
}

func TestCheckpointCreateModeLocalContainerRequiresCloudID(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "local-container"
	server := Server{Provider: "local-container"}
	target := SSHTarget{TargetOS: targetLinux}
	if got := checkpointCreateMode("auto", "", cfg, server, target, false); got != checkpointKindArchive {
		t.Fatalf("no cloud ID auto mode=%q, want %q", got, checkpointKindArchive)
	}
}

func TestDirectAWSCheckpointConfigUsesDirectMarker(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "crabbox.yaml")
	if err := os.WriteFile(cfgPath, []byte("provider: aws\naws:\n  region: us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", cfgPath)
	record := checkpointRecord{
		Kind:     checkpointKindAWSAMI,
		Provider: "aws",
	}
	record.Native.Provider = "aws"
	record.Native.Region = "eu-west-1"
	record.Native.Direct = true

	cfg, ok := directAWSCheckpointConfig(record)
	if !ok {
		t.Fatal("direct AWS checkpoint config not detected")
	}
	if cfg.AWSRegion != "eu-west-1" {
		t.Fatalf("AWSRegion=%q, want record region", cfg.AWSRegion)
	}

	if err := os.WriteFile(cfgPath, []byte("provider: aws\ncoordinator: https://coordinator.example\naws:\n  region: us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, ok = directAWSCheckpointConfig(record)
	if !ok {
		t.Fatal("direct AWS checkpoint should still use direct cleanup when a coordinator is configured later")
	}
	if cfg.Coordinator == "" {
		t.Fatal("expected loaded config to preserve coordinator for unrelated settings")
	}

	if err := os.WriteFile(cfgPath, []byte("provider: aws\naws:\n  region: us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record.Native.Direct = false
	if _, ok := directAWSCheckpointConfig(record); ok {
		t.Fatal("brokered AWS checkpoint should not use direct AWS cleanup")
	}
}

func TestVerifyDirectAWSCheckpointRefusesAccountMismatchBeforeNotFound(t *testing.T) {
	var describeHits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		switch action := r.Form.Get("Action"); action {
		case "GetCallerIdentity":
			writeSTSXML(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>999999999999</Account><Arn>arn:aws:iam::999999999999:user/test</Arn><UserId>AIDAEXAMPLE</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>`)
		case "DescribeImages":
			describeHits++
			writeEC2Error(w, "InvalidAMIID.NotFound", "image not found", http.StatusBadRequest)
		default:
			writeEC2Error(w, "Unexpected", action, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	audit := verifyDirectAWSCheckpointWithClient(context.Background(), checkpointAudit{}, testAWSClient(server.URL), "ami-12345678", "123456789012")
	if audit.ProviderState != "unknown" || audit.NextAction != "check_auth_or_provider" {
		t.Fatalf("audit=%#v, want account mismatch auth/provider state", audit)
	}
	if !strings.Contains(audit.Error, "account mismatch") {
		t.Fatalf("error=%q, want account mismatch", audit.Error)
	}
	if describeHits != 0 {
		t.Fatalf("DescribeImages called %d time(s), want zero", describeHits)
	}
}

func TestCreateDirectAWSAMICheckpointValidatesConfigBeforePreparingSource(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Coordinator = ""
	cfg.AWSRegion = ""
	target := SSHTarget{User: "nobody", Host: "127.0.0.1", Port: "1", TargetOS: targetMacOS}
	app := App{Stderr: io.Discard}

	_, err := app.createDirectAWSAMICheckpoint(context.Background(), cfg, Server{Provider: "aws", CloudID: "i-123"}, target, "cbx_test", "", "repo", false, false, time.Minute)
	if err == nil {
		t.Fatal("expected missing AWS region error")
	}
	if !strings.Contains(err.Error(), "CRABBOX_AWS_REGION or AWS_REGION is required") {
		t.Fatalf("err=%v, want AWS config validation before source preparation", err)
	}
	if strings.Contains(err.Error(), "prepare native checkpoint source") {
		t.Fatalf("source was prepared before AWS config validation: %v", err)
	}
}

func TestWaitForDirectAWSImagePreservesAccountID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if action := r.Form.Get("Action"); action != "DescribeImages" {
			writeEC2Error(w, "Unexpected", action, http.StatusBadRequest)
			return
		}
		writeEC2XML(w, `<DescribeImagesResponse><imagesSet><item><imageId>ami-12345678</imageId><name>checkpoint</name><imageState>available</imageState></item></imagesSet></DescribeImagesResponse>`)
	}))
	defer server.Close()

	image, err := waitForDirectAWSImage(context.Background(), testAWSClient(server.URL), "ami-12345678", "123456789012", time.Second, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if image.AccountID != "123456789012" {
		t.Fatalf("AccountID=%q, want preserved caller account", image.AccountID)
	}
}

func TestApplyNativeImageCheckpointRecordPersistsSnapshotIDs(t *testing.T) {
	record := checkpointRecord{Kind: checkpointKindArchive, Provider: "aws"}
	applyNativeImageCheckpointRecord(&record, CoordinatorImage{
		ID:          "ami-12345678",
		Name:        "checkpoint",
		State:       "available",
		Provider:    "aws",
		Kind:        checkpointKindAWSAMI,
		Region:      "eu-west-1",
		AccountID:   "123456789012",
		ResourceID:  "ami-12345678",
		SnapshotIDs: []string{"snap-1", "snap-2"},
		Direct:      true,
	}, true)

	if record.Kind != checkpointKindAWSAMI {
		t.Fatalf("Kind=%q, want %q", record.Kind, checkpointKindAWSAMI)
	}
	if got := strings.Join(record.Native.SnapshotIDs, ","); got != "snap-1,snap-2" {
		t.Fatalf("snapshot IDs=%q, want snap-1,snap-2", got)
	}
	if record.Native.AccountID != "123456789012" {
		t.Fatalf("AccountID=%q, want caller account", record.Native.AccountID)
	}
	if !record.Native.Direct {
		t.Fatal("direct marker was not persisted")
	}
}

func TestCheckpointCreateModeNativeUsesResolvedProvider(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetLinux
	server := Server{Provider: "hetzner", CloudID: "123"}
	if got := checkpointCreateMode("native", "", cfg, server, SSHTarget{TargetOS: targetLinux}, false); got != "unsupported" {
		t.Fatalf("mode=%q, want unsupported", got)
	}
}

func TestCheckpointCreateModeDirectAWSMacOSDiskSnapshotUsesAMI(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Coordinator = ""
	cfg.TargetOS = targetMacOS
	server := Server{Provider: "aws", CloudID: "i-1234567890abcdef0"}
	target := SSHTarget{TargetOS: targetMacOS}

	for _, mode := range []string{"native", "snapshot"} {
		if got := checkpointCreateMode(mode, checkpointStrategyDiskSnapshot, cfg, server, target, false); got != checkpointKindAWSAMI {
			t.Fatalf("mode=%s got %q, want %q", mode, got, checkpointKindAWSAMI)
		}
	}
}

func TestCheckpointCreateModeFallsBackToArchiveForSSH(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "ssh"
	cfg.TargetOS = targetLinux
	if got := checkpointCreateMode("auto", "", cfg, Server{Provider: "ssh"}, SSHTarget{TargetOS: targetLinux}, false); got != checkpointKindArchive {
		t.Fatalf("mode=%q", got)
	}
}

func TestCreateAWSAMICheckpointValidatesAdminBeforeCloudInit(t *testing.T) {
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "https://coordinator.example")
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "")
	t.Setenv("CRABBOX_ADMIN_TOKEN", "")
	cfg := baseConfig()
	cfg.Coordinator = "https://coordinator.example"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (App{Stdout: io.Discard, Stderr: io.Discard}).createAWSAMICheckpoint(ctx, cfg, SSHTarget{TargetOS: targetLinux}, "cbx_123", "", "repo", true, false, 0)
	if err == nil {
		t.Fatal("expected missing admin token to fail")
	}
	if !strings.Contains(err.Error(), "adminToken") {
		t.Fatalf("err=%v, want admin validation before cloud-init", err)
	}
}

func TestCreateNativeCheckpointRejectsAzureImageBeforeAdminAndCloudInit(t *testing.T) {
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "https://coordinator.example")
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "")
	t.Setenv("CRABBOX_ADMIN_TOKEN", "")
	cfg := baseConfig()
	cfg.Coordinator = "https://coordinator.example"
	cfg.TargetOS = targetLinux

	_, _, err := (App{Stdout: io.Discard, Stderr: io.Discard}).createNativeCheckpoint(
		context.Background(),
		cfg,
		Server{Provider: "azure", CloudID: "crabbox-source"},
		SSHTarget{TargetOS: targetLinux},
		"cbx_123",
		"",
		"repo",
		"",
		checkpointStrategyImage,
		true,
		false,
		0,
	)
	if err == nil {
		t.Fatal("expected Azure image strategy to fail")
	}
	if !strings.Contains(err.Error(), "Azure managed images require") {
		t.Fatalf("err=%v", err)
	}
}

func TestRemotePrepareNativeImageCommandFlushesFilesystem(t *testing.T) {
	cmd := remotePrepareNativeImageCommand()
	for _, want := range []string{"cloud-init clean --logs", "sync"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command missing %q: %s", want, cmd)
		}
	}
}

func TestApplyAWSAMIImageCheckpointRecord(t *testing.T) {
	record := checkpointRecord{Kind: checkpointKindArchive}
	applyAWSAMIImageCheckpointRecord(&record, CoordinatorImage{
		ID:     "ami-12345678",
		Name:   "checkpoint",
		State:  "pending",
		Region: "us-east-2",
	}, true)

	if record.Kind != checkpointKindAWSAMI || record.Native.Provider != "aws" {
		t.Fatalf("kind/provider not applied: %#v", record)
	}
	if record.Native.ImageID != "ami-12345678" || record.Native.Region != "us-east-2" || !record.Native.NoReboot {
		t.Fatalf("native image not applied: %#v", record.Native)
	}
}

func TestNativeCheckpointForkWorkdirHonorsOverride(t *testing.T) {
	cfg := defaultConfig()
	cfg.WorkRoot = "/work"
	if got := nativeCheckpointForkWorkdir(cfg, "cbx_new", "my-app", " /tmp/repro "); got != "/tmp/repro" {
		t.Fatalf("workdir=%q, want override", got)
	}
	if got := nativeCheckpointForkWorkdir(cfg, "cbx_new", "my-app", ""); got != "/work/cbx_new/my-app" {
		t.Fatalf("workdir=%q, want default lease workdir", got)
	}
}

func TestCheckpointForkReleasesLeaseWhenKeepFalse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	backend := &checkpointForkReleaseBackend{leaseID: "cbx_fork_keep_false"}
	testAWSBackendOverride = backend
	defer func() { testAWSBackendOverride = nil }()

	repo, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	record := checkpointRecord{
		ID:          "chk_keep_false",
		Kind:        checkpointKindAWSAMI,
		TargetOS:    targetLinux,
		WindowsMode: windowsModeNormal,
		Workdir:     remoteJoin(cfg, backend.leaseID, repo.Name),
	}
	record.Native.ImageID = "ami-12345678"
	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(record); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.checkpointFork(context.Background(), []string{record.ID, "--keep=false", "--slug", "Fork Smoke"}); err != nil {
		t.Fatal(err)
	}
	if backend.acquireKeep {
		t.Fatal("acquire Keep=true, want false")
	}
	if backend.acquireSlug != "fork-smoke" {
		t.Fatalf("acquire slug=%q, want fork-smoke", backend.acquireSlug)
	}
	if backend.releaseCount != 1 {
		t.Fatalf("releaseCount=%d, want 1", backend.releaseCount)
	}
}

func TestCheckpointForkRejectsPendingNativeCheckpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")

	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record := checkpointRecord{
		ID:        "chk_pending_native",
		Kind:      checkpointKindAWSEBS,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		TargetOS:  targetLinux,
	}
	if _, err := store.Create(record); err != nil {
		t.Fatal(err)
	}

	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err = app.checkpointFork(context.Background(), []string{record.ID})
	if err == nil {
		t.Fatal("expected pending native checkpoint fork to fail")
	}
	if !strings.Contains(err.Error(), "pending") {
		t.Fatalf("err=%v, want pending checkpoint error", err)
	}
}

func TestCheckpointInspectVerifyResourceOnlyNativeDoesNotUseCoordinator(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")

	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record := checkpointRecord{
		ID:        "chk_resource_only",
		Kind:      checkpointKindGCPDisk,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		TargetOS:  targetLinux,
	}
	record.Native.Resource = "projects/proj/global/snapshots/checkpoint"
	if _, err := store.Create(record); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.checkpointInspect(context.Background(), []string{record.ID, "--verify", "--json"}); err != nil {
		t.Fatal(err)
	}
	var audit checkpointAudit
	if err := json.Unmarshal(stdout.Bytes(), &audit); err != nil {
		t.Fatal(err)
	}
	if audit.ProviderState != "unverified_ref" || audit.NextAction != "fork_or_delete_local" {
		t.Fatalf("audit=%#v", audit)
	}
}

func TestCheckpointInspectVerifyDirectAWSUsesLocalPathBeforeCoordinator(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_AWS_REGION", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	var coordinatorHits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		coordinatorHits++
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "admin")
	cfgPath := filepath.Join(t.TempDir(), "crabbox.yaml")
	if err := os.WriteFile(cfgPath, []byte("provider: aws\ncoordinator: "+server.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", cfgPath)

	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record := checkpointRecord{
		ID:        "chk_direct_aws",
		Kind:      checkpointKindAWSAMI,
		Provider:  "aws",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		TargetOS:  targetMacOS,
	}
	record.Native.Provider = "aws"
	record.Native.ImageID = "ami-12345678"
	record.Native.Region = "not a valid region"
	record.Native.Direct = true
	if _, err := store.Create(record); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.checkpointInspect(context.Background(), []string{record.ID, "--verify", "--json"}); err != nil {
		t.Fatal(err)
	}
	var audit checkpointAudit
	if err := json.Unmarshal(stdout.Bytes(), &audit); err != nil {
		t.Fatal(err)
	}
	if coordinatorHits != 0 {
		t.Fatalf("direct AWS verification hit coordinator %d time(s)", coordinatorHits)
	}
	if audit.ProviderState != "unknown" || audit.NextAction != "check_auth_or_provider" {
		t.Fatalf("audit=%#v", audit)
	}
	if audit.Error == "" {
		t.Fatal("expected local AWS verification error")
	}
}

func TestCheckpointDeleteResourceOnlyNativeDeletesProviderResource(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))

	var deleteRequest string
	var deleteProvider string
	var deleteKind string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deleteRequest = r.Method + " " + r.RequestURI
		deleteProvider = r.URL.Query().Get("provider")
		deleteKind = r.URL.Query().Get("kind")
		_, _ = w.Write([]byte(`{"deleted":true}`))
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "admin")

	store, err := defaultCheckpointStore()
	if err != nil {
		t.Fatal(err)
	}
	record := checkpointRecord{
		ID:        "chk_resource_delete",
		Kind:      checkpointKindAzureOS,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		TargetOS:  targetLinux,
	}
	record.Native.Resource = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/snapshots/checkpoint"
	if _, err := store.Create(record); err != nil {
		t.Fatal(err)
	}

	app := App{Stdout: io.Discard, Stderr: io.Discard}
	if err := app.checkpointDelete(context.Background(), []string{record.ID}); err != nil {
		t.Fatal(err)
	}
	if deleteProvider != "azure" {
		t.Fatalf("provider query=%q", deleteProvider)
	}
	if deleteKind != "azure-os-disk-snapshot" {
		t.Fatalf("kind query=%q", deleteKind)
	}
	if !strings.HasPrefix(deleteRequest, "DELETE /v1/images/%2Fsubscriptions%2Fsub%2FresourceGroups%2Frg%2Fproviders%2FMicrosoft.Compute%2Fsnapshots%2Fcheckpoint?") {
		t.Fatalf("delete request=%q", deleteRequest)
	}
	if _, _, err := store.Read(record.ID); err == nil {
		t.Fatal("delete kept checkpoint")
	}
}

func TestNativeCheckpointResourceIDAllowsAzureGCPResourceOnlyRecords(t *testing.T) {
	aws := checkpointRecord{Kind: checkpointKindAWSAMI}
	aws.Native.Resource = "ami-resource-only"
	if got := nativeCheckpointResourceID(aws); got != "" {
		t.Fatalf("aws resource-only ref=%q, want empty", got)
	}
	aws.Native.ImageID = "ami-12345678"
	if got := nativeCheckpointResourceID(aws); got != "ami-12345678" {
		t.Fatalf("aws ref=%q", got)
	}

	azure := checkpointRecord{Kind: checkpointKindAzureOS}
	azure.Native.Resource = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/snapshots/checkpoint"
	if got := nativeCheckpointResourceID(azure); got != azure.Native.Resource {
		t.Fatalf("azure ref=%q", got)
	}

	gcp := checkpointRecord{Kind: checkpointKindGCPDisk}
	gcp.Native.Resource = "projects/proj/global/snapshots/checkpoint"
	if got := nativeCheckpointResourceID(gcp); got != gcp.Native.Resource {
		t.Fatalf("gcp ref=%q", got)
	}
}

type checkpointForkReleaseBackend struct {
	leaseID      string
	acquireKeep  bool
	acquireSlug  string
	releaseCount int
}

func (b *checkpointForkReleaseBackend) Spec() ProviderSpec {
	return testAWSProvider{}.Spec()
}

func (b *checkpointForkReleaseBackend) Acquire(_ context.Context, req AcquireRequest) (LeaseTarget, error) {
	b.acquireKeep = req.Keep
	b.acquireSlug = req.RequestedSlug
	return LeaseTarget{
		Server:  Server{Provider: "aws", CloudID: "i-123", Labels: map[string]string{}},
		SSH:     SSHTarget{User: "crabbox", Port: "22", TargetOS: targetLinux},
		LeaseID: b.leaseID,
	}, nil
}

func (b *checkpointForkReleaseBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return b.Acquire(context.Background(), AcquireRequest{})
}

func (b *checkpointForkReleaseBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, nil
}

func (b *checkpointForkReleaseBackend) ReleaseLease(context.Context, ReleaseLeaseRequest) error {
	b.releaseCount++
	return nil
}

func (b *checkpointForkReleaseBackend) Touch(context.Context, TouchRequest) (Server, error) {
	return Server{Provider: "aws", Labels: map[string]string{}}, nil
}

func TestApplyAWSAMICheckpointForkConfigRecomputesServerType(t *testing.T) {
	fs := newFlagSet("checkpoint fork", io.Discard)
	_ = fs.String("type", "", "provider type")
	cfg := defaultConfig()
	cfg.Provider = "hetzner"
	cfg.Class = "beast"
	cfg.ServerType = "ccx63"
	cfg.ServerTypeExplicit = true
	cfg.CoordAdminToken = "admin-token"
	record := checkpointRecord{Kind: checkpointKindAWSAMI, TargetOS: targetLinux, WindowsMode: windowsModeNormal}
	record.Native.ImageID = "ami-12345678"
	record.Native.Region = "eu-west-1"

	if err := applyAWSAMICheckpointForkConfig(&cfg, fs, record); err != nil {
		t.Fatal(err)
	}

	if cfg.Provider != "aws" || cfg.AWSAMI != "ami-12345678" || cfg.AWSRegion != "eu-west-1" {
		t.Fatalf("aws config not applied: %#v", cfg)
	}
	if cfg.CoordToken != "admin-token" {
		t.Fatalf("coord token=%q, want admin token for native checkpoint fork", cfg.CoordToken)
	}
	if cfg.ServerTypeExplicit {
		t.Fatal("ServerTypeExplicit=true, want false")
	}
	if cfg.ServerType != "c7a.48xlarge" {
		t.Fatalf("ServerType=%q, want AWS beast default", cfg.ServerType)
	}
}

func TestApplyAWSAMICheckpointForkConfigKeepsDirectRecordsOffCoordinator(t *testing.T) {
	fs := newFlagSet("checkpoint fork", io.Discard)
	_ = fs.String("type", "", "provider type")
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Coordinator = "https://coordinator.example"
	cfg.CoordToken = "user-token"
	cfg.CoordAdminToken = "admin-token"
	record := checkpointRecord{Kind: checkpointKindAWSAMI, TargetOS: targetLinux, WindowsMode: windowsModeNormal}
	record.Native.Provider = "aws"
	record.Native.ImageID = "ami-12345678"
	record.Native.Region = "eu-west-1"
	record.Native.Direct = true

	if err := applyAWSAMICheckpointForkConfig(&cfg, fs, record); err != nil {
		t.Fatal(err)
	}
	if cfg.Coordinator != "" || cfg.CoordToken != "" {
		t.Fatalf("direct checkpoint fork kept coordinator: coordinator=%q token=%q", cfg.Coordinator, cfg.CoordToken)
	}
	if cfg.AWSAMI != "ami-12345678" || cfg.AWSRegion != "eu-west-1" {
		t.Fatalf("direct AWS image config not applied: %#v", cfg)
	}
}

func TestApplyAWSAMICheckpointForkConfigPreservesDirectMacHostPin(t *testing.T) {
	fs := newFlagSet("checkpoint fork", io.Discard)
	_ = fs.String("type", "", "provider type")
	_ = fs.String("market", "spot", "capacity market")
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Coordinator = "https://coordinator.example"
	cfg.TargetOS = targetLinux
	record := checkpointRecord{
		Kind:        checkpointKindAWSAMI,
		TargetOS:    targetMacOS,
		WindowsMode: windowsModeNormal,
		ServerType:  "mac2.metal",
		HostID:      "h-000000000001",
	}
	record.Native.Provider = "aws"
	record.Native.ImageID = "ami-12345678"
	record.Native.Region = "eu-west-1"
	record.Native.Direct = true

	if err := applyAWSAMICheckpointForkConfig(&cfg, fs, record); err != nil {
		t.Fatal(err)
	}

	if cfg.Coordinator != "" {
		t.Fatalf("direct checkpoint fork kept coordinator: %q", cfg.Coordinator)
	}
	if cfg.HostID != "h-000000000001" || cfg.AWSMacHostID != "h-000000000001" {
		t.Fatalf("host pin not preserved: hostID=%q awsMacHostID=%q", cfg.HostID, cfg.AWSMacHostID)
	}
	if cfg.Capacity.Market != "on-demand" {
		t.Fatalf("market=%q, want on-demand", cfg.Capacity.Market)
	}
}

func TestApplyAWSAMICheckpointForkConfigHonorsClassOverride(t *testing.T) {
	fs := newFlagSet("checkpoint fork", io.Discard)
	class := fs.String("class", "standard", "provider class")
	_ = fs.String("type", "", "provider type")
	if err := parseFlags(fs, []string{"--class", "beast"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Provider = "hetzner"
	cfg.Class = *class
	cfg.ServerType = "ccx63"
	cfg.ServerTypeExplicit = true
	record := checkpointRecord{
		Kind:        checkpointKindAWSAMI,
		TargetOS:    targetLinux,
		WindowsMode: windowsModeNormal,
		ServerType:  "c7a.4xlarge",
	}
	record.Native.ImageID = "ami-12345678"
	record.Native.Region = "eu-west-1"

	if err := applyAWSAMICheckpointForkConfig(&cfg, fs, record); err != nil {
		t.Fatal(err)
	}

	if cfg.Provider != "aws" || cfg.AWSAMI != "ami-12345678" || cfg.AWSRegion != "eu-west-1" {
		t.Fatalf("aws config not applied: %#v", cfg)
	}
	if cfg.ServerTypeExplicit {
		t.Fatal("ServerTypeExplicit=true, want false")
	}
	if cfg.ServerType != "c7a.48xlarge" {
		t.Fatalf("ServerType=%q, want AWS beast default instead of checkpoint source type", cfg.ServerType)
	}
}

func TestApplyAWSAMICheckpointForkConfigPreservesExplicitTypeFlag(t *testing.T) {
	fs := newFlagSet("checkpoint fork", io.Discard)
	serverType := fs.String("type", "", "provider type")
	if err := parseFlags(fs, []string{"--type", "c7a.4xlarge"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Provider = "hetzner"
	cfg.ServerType = *serverType
	cfg.ServerTypeExplicit = true
	record := checkpointRecord{Kind: checkpointKindAWSAMI, TargetOS: targetLinux, WindowsMode: windowsModeNormal}
	record.Native.ImageID = "ami-12345678"

	if err := applyAWSAMICheckpointForkConfig(&cfg, fs, record); err != nil {
		t.Fatal(err)
	}

	if cfg.ServerType != "c7a.4xlarge" || !cfg.ServerTypeExplicit {
		t.Fatalf("explicit type not preserved: type=%q explicit=%t", cfg.ServerType, cfg.ServerTypeExplicit)
	}
}

func TestApplyAWSMacOSCheckpointForkConfigPreservesTypeWithoutHostPin(t *testing.T) {
	fs := newFlagSet("checkpoint fork", io.Discard)
	_ = fs.String("type", "", "provider type")
	_ = fs.String("market", "spot", "capacity market")
	cfg := defaultConfig()
	cfg.Provider = "hetzner"
	cfg.Class = "standard"
	cfg.Capacity.Market = "spot"
	record := checkpointRecord{
		Kind:        checkpointKindAWSEBS,
		TargetOS:    targetMacOS,
		WindowsMode: windowsModeNormal,
		ServerType:  "mac2-m2pro.metal",
		HostID:      "h-000000000001",
	}
	record.Native.ImageID = "snap-000000000001"
	record.Native.Region = "eu-west-1"

	applyNativeCheckpointForkConfig(&cfg, fs, record)

	if cfg.Provider != "aws" || cfg.TargetOS != targetMacOS || cfg.AWSSnapshot != "snap-000000000001" {
		t.Fatalf("aws macOS snapshot config not applied: %#v", cfg)
	}
	if cfg.HostID != "" || cfg.AWSMacHostID != "" {
		t.Fatalf("host pin carried into fork: hostID=%q awsMacHostID=%q", cfg.HostID, cfg.AWSMacHostID)
	}
	if cfg.ServerType != "mac2-m2pro.metal" || !cfg.ServerTypeExplicit {
		t.Fatalf("server type not preserved: type=%q explicit=%t", cfg.ServerType, cfg.ServerTypeExplicit)
	}
	if cfg.Capacity.Market != "on-demand" {
		t.Fatalf("market=%q, want on-demand", cfg.Capacity.Market)
	}
	if cfg.WorkRoot != defaultMacOSWorkRoot {
		t.Fatalf("WorkRoot=%q, want %q", cfg.WorkRoot, defaultMacOSWorkRoot)
	}
}

func TestApplyNativeCheckpointForkConfigForAzureAndGCP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record checkpointRecord
		check  func(t *testing.T, cfg Config)
	}{
		{
			name: "azure",
			record: func() checkpointRecord {
				record := checkpointRecord{Kind: checkpointKindAzure, TargetOS: targetLinux}
				record.Native.ImageID = "checkpoint-azure"
				record.Native.Resource = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/images/checkpoint-azure"
				record.Native.Region = "eastus"
				return record
			}(),
			check: func(t *testing.T, cfg Config) {
				if cfg.Provider != "azure" || cfg.AzureLocation != "eastus" || cfg.AzureImage == "" {
					t.Fatalf("azure config not applied: %#v", cfg)
				}
			},
		},
		{
			name: "azure disk snapshot",
			record: func() checkpointRecord {
				record := checkpointRecord{Kind: checkpointKindAzureOS, TargetOS: targetLinux}
				record.Native.ImageID = "checkpoint-azure"
				record.Native.Resource = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/snapshots/checkpoint-azure"
				record.Native.Region = "eastus"
				return record
			}(),
			check: func(t *testing.T, cfg Config) {
				if cfg.Provider != "azure" || cfg.AzureLocation != "eastus" || cfg.AzureSnapshot == "" {
					t.Fatalf("azure snapshot config not applied: %#v", cfg)
				}
			},
		},
		{
			name: "gcp",
			record: func() checkpointRecord {
				record := checkpointRecord{Kind: checkpointKindGCP, TargetOS: targetLinux}
				record.Native.ImageID = "checkpoint-gcp"
				record.Native.Resource = "projects/proj/global/machineImages/checkpoint-gcp"
				record.Native.Region = "us-central1-a"
				record.Native.Project = "proj"
				return record
			}(),
			check: func(t *testing.T, cfg Config) {
				if cfg.Provider != "gcp" || cfg.GCPZone != "us-central1-a" || cfg.GCPProject != "proj" || cfg.GCPMachineImage == "" || !cfg.gcpProjectExplicit {
					t.Fatalf("gcp config not applied: %#v", cfg)
				}
			},
		},
		{
			name: "gcp disk snapshot",
			record: func() checkpointRecord {
				record := checkpointRecord{Kind: checkpointKindGCPDisk, TargetOS: targetLinux}
				record.Native.ImageID = "checkpoint-gcp"
				record.Native.Resource = "projects/proj/global/snapshots/checkpoint-gcp"
				record.Native.Region = "us-central1-a"
				record.Native.Project = "proj"
				return record
			}(),
			check: func(t *testing.T, cfg Config) {
				if cfg.Provider != "gcp" || cfg.GCPZone != "us-central1-a" || cfg.GCPProject != "proj" || cfg.GCPSnapshot == "" || !cfg.gcpProjectExplicit {
					t.Fatalf("gcp snapshot config not applied: %#v", cfg)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFlagSet("checkpoint fork", io.Discard)
			_ = fs.String("type", "", "provider type")
			cfg := defaultConfig()
			cfg.Provider = "hetzner"
			cfg.Class = "standard"
			if err := applyNativeCheckpointForkConfig(&cfg, fs, tc.record); err != nil {
				t.Fatal(err)
			}
			tc.check(t, cfg)
			if cfg.ServerTypeExplicit {
				t.Fatal("ServerTypeExplicit=true, want false")
			}
		})
	}
}

func TestApplyNativeCheckpointForkConfigForParallelsPreservesLinkedCloneMode(t *testing.T) {
	fs := newFlagSet("checkpoint fork", io.Discard)
	_ = fs.String("type", "", "provider type")
	_ = fs.String("parallels-clone-mode", "", "Parallels clone mode")
	cfg := baseConfig()
	cfg.Provider = "hetzner"
	record := checkpointRecord{Kind: checkpointKindParallels, TargetOS: targetMacOS}
	record.Native.ImageID = "{snap1}"
	record.Native.Resource = "vm1"
	record.Native.State = "poweron"
	record.Native.Region = "mac-host"

	if err := applyNativeCheckpointForkConfig(&cfg, fs, record); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "parallels" || cfg.Parallels.SourceID != "vm1" || cfg.Parallels.SourceSnapshotID != "{snap1}" || cfg.Parallels.Host != "mac-host" {
		t.Fatalf("parallels config not applied: %#v", cfg)
	}
	if cfg.Parallels.CloneMode != "linked" {
		t.Fatalf("snapshot forks should preserve linked clone mode, got %q", cfg.Parallels.CloneMode)
	}

	cfg = baseConfig()
	cfg.Provider = "hetzner"
	cfg.Parallels.CloneMode = "linked"
	fs = newFlagSet("checkpoint fork", io.Discard)
	_ = fs.String("type", "", "provider type")
	_ = fs.String("parallels-clone-mode", "", "Parallels clone mode")
	if err := parseFlags(fs, []string{"--parallels-clone-mode", "linked"}); err != nil {
		t.Fatal(err)
	}
	if err := applyNativeCheckpointForkConfig(&cfg, fs, record); err != nil {
		t.Fatal(err)
	}
	if cfg.Parallels.CloneMode != "linked" {
		t.Fatalf("explicit clone mode should be preserved, got %q", cfg.Parallels.CloneMode)
	}
}

func TestApplyNativeCheckpointForkConfigHonorsAzureOSDiskFlagAfterProviderRewrite(t *testing.T) {
	fs := newFlagSet("checkpoint fork", io.Discard)
	_ = fs.String("type", "", "provider type")
	_ = fs.String("azure-os-disk", AzureOSDiskManaged, "Azure OS disk mode")
	if err := parseFlags(fs, []string{"--azure-os-disk", "ephemeral"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Provider = "hetzner"
	cfg.AzureOSDisk = AzureOSDiskManaged
	record := checkpointRecord{Kind: checkpointKindAzureOS, TargetOS: targetLinux}
	record.Native.ImageID = "checkpoint-azure"

	if err := applyNativeCheckpointForkConfig(&cfg, fs, record); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "azure" {
		t.Fatalf("Provider=%q", cfg.Provider)
	}
	if cfg.AzureOSDisk != AzureOSDiskEphemeral || !cfg.AzureOSDiskExplicit {
		t.Fatalf("AzureOSDisk=%q explicit=%t", cfg.AzureOSDisk, cfg.AzureOSDiskExplicit)
	}
}

func TestApplyNativeCheckpointForkConfigHonorsEmptyAzureOSDiskFlag(t *testing.T) {
	fs := newFlagSet("checkpoint fork", io.Discard)
	_ = fs.String("type", "", "provider type")
	_ = fs.String("azure-os-disk", AzureOSDiskManaged, "Azure OS disk mode")
	if err := parseFlags(fs, []string{"--azure-os-disk="}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Provider = "hetzner"
	cfg.AzureOSDisk = AzureOSDiskEphemeral
	cfg.AzureOSDiskExplicit = true
	record := checkpointRecord{Kind: checkpointKindAzureOS, TargetOS: targetLinux}
	record.Native.ImageID = "checkpoint-azure"

	if err := applyNativeCheckpointForkConfig(&cfg, fs, record); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "azure" {
		t.Fatalf("Provider=%q", cfg.Provider)
	}
	if cfg.AzureOSDisk != AzureOSDiskManaged || !cfg.AzureOSDiskExplicit {
		t.Fatalf("AzureOSDisk=%q explicit=%t", cfg.AzureOSDisk, cfg.AzureOSDiskExplicit)
	}
}

func TestParseInterspersedFlagsAllowsCheckpointBeforeFlags(t *testing.T) {
	fs := newFlagSet("checkpoint restore", io.Discard)
	id := fs.String("id", "", "lease id")
	clear := fs.Bool("clear", true, "clear")
	if err := parseInterspersedFlags(fs, []string{"chk_123", "--id", "cbx_123", "--clear=false"}); err != nil {
		t.Fatal(err)
	}
	if *id != "cbx_123" || *clear {
		t.Fatalf("flags id=%q clear=%t", *id, *clear)
	}
	if fs.NArg() != 1 || fs.Arg(0) != "chk_123" {
		t.Fatalf("args=%q", fs.Args())
	}
}

func TestRemoteCheckpointArchiveCommand(t *testing.T) {
	cmd := remoteCheckpointArchiveCommand("/work/cbx_123/my app")
	for _, want := range []string{
		"test -d",
		"/work/cbx_123/my app",
		"tar -C",
		"--exclude",
		"./.crabbox/env",
		"./.crabbox/scripts",
		"-czf - .",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command missing %q: %s", want, cmd)
		}
	}
}

func TestRemoteCheckpointRestoreCommandClearsWorkdir(t *testing.T) {
	cmd := remoteCheckpointRestoreCommand("/work/repo", true)
	for _, want := range []string{
		"mktemp /tmp/crabbox-checkpoint.XXXXXX",
		"trap cleanup EXIT INT TERM",
		"cat > \"$tmp\"",
		"mkdir -p",
		"/work/repo",
		"find",
		"-mindepth 1 -maxdepth 1 -exec rm -rf -- {} +",
		"tar -C",
		"-xzf",
		"rm -f --",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command missing %q: %s", want, cmd)
		}
	}
}

func TestRemoteRelocateNativeCheckpointWorkdirCommand(t *testing.T) {
	cmd := remoteRelocateNativeCheckpointWorkdirCommand("/work/cbx_old/app", "/work/cbx_new/app")
	for _, want := range []string{
		"/work/cbx_old/app",
		"/work/cbx_new/app",
		"test -d",
		"mkdir -p",
		"mv",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command missing %q: %s", want, cmd)
		}
	}
	if got := remoteRelocateNativeCheckpointWorkdirCommand("/work/app", "/work/app"); got != "" {
		t.Fatalf("same workdir command=%q, want empty", got)
	}
}

type checkpointParallelsRunner struct {
	vmState       string
	snapshotState string
	snapshotName  string
	commands      []string
}

func (r *checkpointParallelsRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	if len(req.Args) == 0 {
		return LocalCommandResult{}, nil
	}
	r.commands = append(r.commands, req.Args[0])
	switch req.Args[0] {
	case "list":
		return LocalCommandResult{Stdout: `[{"ID":"vm1","Name":"test-vm","State":"` + r.vmState + `"}]`}, nil
	case "snapshot":
		for i, arg := range req.Args {
			if arg == "--name" && i+1 < len(req.Args) {
				r.snapshotName = req.Args[i+1]
				break
			}
		}
		return LocalCommandResult{}, nil
	case "snapshot-list":
		return LocalCommandResult{Stdout: `{"{snap1}":{"name":"` + r.snapshotName + `","state":"` + r.snapshotState + `"}}`}, nil
	default:
		return LocalCommandResult{}, nil
	}
}

func (r *checkpointParallelsRunner) called(name string) bool {
	for _, command := range r.commands {
		if command == name {
			return true
		}
	}
	return false
}
