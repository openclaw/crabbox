package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	firstDir, err := checkpointDir("chk_first")
	if err != nil {
		t.Fatal(err)
	}
	secondDir, err := checkpointDir("chk_second")
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
	if err := writeCheckpointRecord(firstDir, first); err != nil {
		t.Fatal(err)
	}
	if err := writeCheckpointRecord(secondDir, second); err != nil {
		t.Fatal(err)
	}
	records, err := listCheckpointRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].ID != "chk_second" || records[1].ID != "chk_first" {
		t.Fatalf("unexpected order: %#v", records)
	}
	got, _, err := readCheckpointRecord("chk_first")
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
	if err := app.checkpointDelete([]string{"chk_missing"}); err == nil {
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
	if err := app.checkpointDelete([]string{"chk_corrupt"}); err == nil {
		t.Fatal("expected corrupt checkpoint delete to fail")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("corrupt checkpoint dir removed: %v", err)
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

func TestDefaultCheckpointRestoreWorkdirUsesTargetLease(t *testing.T) {
	cfg := defaultConfig()
	cfg.WorkRoot = "/work"
	got := defaultCheckpointRestoreWorkdir(cfg, "cbx_new", "my-app", "/work/cbx_old/my-app")
	if got != "/work/cbx_new/my-app" {
		t.Fatalf("restore workdir = %q, want target lease workdir", got)
	}
}

func TestCheckpointCreateModePrefersDiskSnapshotLinuxNative(t *testing.T) {
	cfg := defaultConfig()
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

func TestCheckpointCreateModeNativeUsesResolvedProvider(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.TargetOS = targetLinux
	server := Server{Provider: "hetzner", CloudID: "123"}
	if got := checkpointCreateMode("native", "", cfg, server, SSHTarget{TargetOS: targetLinux}, false); got != "unsupported" {
		t.Fatalf("mode=%q, want unsupported", got)
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

	_, err := (App{Stdout: io.Discard, Stderr: io.Discard}).createNativeCheckpoint(
		context.Background(),
		cfg,
		Server{Provider: "azure", CloudID: "crabbox-source"},
		SSHTarget{TargetOS: targetLinux},
		"cbx_123",
		"",
		"repo",
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

func TestRemotePrepareAWSAMICommandFlushesFilesystem(t *testing.T) {
	cmd := remotePrepareAWSAMICommand()
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
	dir, err := checkpointDir(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCheckpointRecord(dir, record); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.checkpointFork(context.Background(), []string{record.ID, "--keep=false"}); err != nil {
		t.Fatal(err)
	}
	if backend.acquireKeep {
		t.Fatal("acquire Keep=true, want false")
	}
	if backend.releaseCount != 1 {
		t.Fatalf("releaseCount=%d, want 1", backend.releaseCount)
	}
}

type checkpointForkReleaseBackend struct {
	leaseID      string
	acquireKeep  bool
	releaseCount int
}

func (b *checkpointForkReleaseBackend) Spec() ProviderSpec {
	return testAWSProvider{}.Spec()
}

func (b *checkpointForkReleaseBackend) Acquire(_ context.Context, req AcquireRequest) (LeaseTarget, error) {
	b.acquireKeep = req.Keep
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

	applyAWSAMICheckpointForkConfig(&cfg, fs, record)

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

	applyAWSAMICheckpointForkConfig(&cfg, fs, record)

	if cfg.ServerType != "c7a.4xlarge" || !cfg.ServerTypeExplicit {
		t.Fatalf("explicit type not preserved: type=%q explicit=%t", cfg.ServerType, cfg.ServerTypeExplicit)
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
			applyNativeCheckpointForkConfig(&cfg, fs, tc.record)
			tc.check(t, cfg)
			if cfg.ServerTypeExplicit {
				t.Fatal("ServerTypeExplicit=true, want false")
			}
		})
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
	cmd := remoteCheckpointRestoreCommand("/work/repo", "/tmp/chk.tar.gz", true)
	for _, want := range []string{
		"mkdir -p",
		"/work/repo",
		"find",
		"-mindepth 1 -maxdepth 1 -exec rm -rf -- {} +",
		"tar -C",
		"-xzf",
		"/tmp/chk.tar.gz",
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
