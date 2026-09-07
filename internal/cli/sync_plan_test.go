package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncPlanDir(t *testing.T) {
	tests := map[string]string{
		"README.md":             ".",
		"docs/README.md":        "docs",
		"packages/app/src/a.ts": "packages/app",
		"apps/foo/.build/a.o":   "apps/foo",
		"worker/src/index.ts":   "worker/src",
	}
	for input, want := range tests {
		if got := syncPlanDir(input); got != want {
			t.Fatalf("syncPlanDir(%q)=%q want %q", input, got, want)
		}
	}
}

func TestSortSyncPlanRows(t *testing.T) {
	rows := []syncPlanRow{{Path: "b", Bytes: 2}, {Path: "a", Bytes: 2}, {Path: "c", Bytes: 3}}
	sortSyncPlanRows(rows)
	got := rows[0].Path + rows[1].Path + rows[2].Path
	if got != "cab" {
		t.Fatalf("sorted rows=%v", rows)
	}
}

func TestSyncPlanJSONOutput(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "")
	t.Setenv("CRABBOX_SYNC_ALLOW_LARGE", "")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, cfgPath, "sync:\n  warnFiles: 1\n  warnBytes: 4\n  failFiles: 2\n  failBytes: 30\n")
	t.Setenv("CRABBOX_CONFIG", cfgPath)

	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "README.md"), "readme")
	writeFile(t, filepath.Join(dir, "assets", "demo.bin"), strings.Repeat("x", 16))
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	if err := os.Remove(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "notes.txt"), "note-data")
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"sync-plan", "--limit", "1", "--json"})
	if err != nil {
		t.Fatalf("sync-plan --json error=%v stderr=%q", err, stderr.String())
	}
	var got syncPlanJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode sync-plan JSON: %v\n%s", err, stdout.String())
	}

	if got.Candidate.Files != 2 || got.Candidate.Bytes != 25 || got.Candidate.HumanBytes != "25 B" {
		t.Fatalf("candidate=%+v", got.Candidate)
	}
	if got.DirtyDelta.Files != 2 || got.DirtyDelta.Bytes != 9 || got.DeletedTrackedPaths != 1 {
		t.Fatalf("dirty=%+v deleted=%d", got.DirtyDelta, got.DeletedTrackedPaths)
	}
	if got.Guardrail.Scope != "dirty_delta" || got.Guardrail.Files != 2 || got.Guardrail.Bytes != 9 || got.Guardrail.Status != "failed" {
		t.Fatalf("guardrail=%+v", got.Guardrail)
	}
	if got.Guardrail.Limits.FailFiles != 2 || got.Guardrail.Limits.WarnBytes != 4 || got.Guardrail.AllowLarge {
		t.Fatalf("guardrail limits=%+v allowLarge=%t", got.Guardrail.Limits, got.Guardrail.AllowLarge)
	}
	for _, want := range []syncPlanJSONGuardrailReason{
		{Status: "failed", Metric: "files", Actual: 2, Limit: 2},
		{Status: "warning", Metric: "files", Actual: 2, Limit: 1},
		{Status: "warning", Metric: "bytes", Actual: 9, Limit: 4},
	} {
		if !syncPlanHasReason(got.Guardrail.Reasons, want) {
			t.Fatalf("guardrail reasons=%+v missing=%+v", got.Guardrail.Reasons, want)
		}
	}
	if len(got.TopFiles) != 1 || got.TopFiles[0].Path != "assets/demo.bin" || got.TopFiles[0].Bytes != 16 || got.TopFiles[0].HumanBytes != "16 B" {
		t.Fatalf("topFiles=%+v", got.TopFiles)
	}
	if len(got.TopDirs) != 1 || got.TopDirs[0].Path != "assets" || got.TopDirs[0].Bytes != 16 {
		t.Fatalf("topDirs=%+v", got.TopDirs)
	}
}

func TestSyncPlanProviderGuardrailMatchesArchivePreflight(t *testing.T) {
	for _, tc := range []struct {
		name, scope, status         string
		full, clean, allow, exclude bool
		failFiles                   int
		failBytes                   int64
	}{
		{name: "full file limit", full: true, failFiles: 2, scope: "candidate", status: "failed"},
		{name: "full byte limit", full: true, failBytes: 10, scope: "candidate", status: "failed"},
		{name: "clean full archive", full: true, clean: true, failFiles: 2, scope: "candidate", status: "failed"},
		{name: "allow large", full: true, allow: true, failFiles: 2, scope: "candidate", status: "ok"},
		{name: "filtered archive", full: true, exclude: true, failFiles: 2, scope: "candidate", status: "ok"},
		{name: "delta transport", failFiles: 2, scope: "dirty_delta", status: "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := isolatedConfigPath(t)
			t.Setenv("CRABBOX_SYNC_ALLOW_LARGE", "")
			provider := &probeAdmissionProvider{spec: ProviderSpec{Name: "sync-plan-scope-test", Kind: ProviderKindDelegatedRun}}
			if tc.full {
				provider.spec.SyncGuardrailFullCandidate = true
			}
			RegisterProvider(provider)
			t.Cleanup(func() { delete(providerRegistry, provider.Name()) })
			config := fmt.Sprintf("provider: %s\nsync:\n  failFiles: %d\n  failBytes: %d\n  allowLarge: %t\n", provider.Name(), tc.failFiles, tc.failBytes, tc.allow)
			if tc.exclude {
				config += "  exclude: [b.txt]\n"
			}
			writeFile(t, configPath, config)
			dir := t.TempDir()
			runGit(t, dir, "init")
			runGit(t, dir, "config", "user.email", "test@example.com")
			runGit(t, dir, "config", "user.name", "Test")
			writeFile(t, filepath.Join(dir, "a.txt"), "aaa")
			writeFile(t, filepath.Join(dir, "b.txt"), "bbbb")
			runGit(t, dir, "add", ".")
			runGit(t, dir, "commit", "-m", "fixture")
			if !tc.clean {
				writeFile(t, filepath.Join(dir, "a.txt"), "changed")
			}
			t.Chdir(dir)
			var stdout, stderr bytes.Buffer
			if err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"sync-plan", "--json"}); err != nil {
				t.Fatalf("sync-plan: %v: %s", err, stderr.String())
			}
			var got syncPlanJSONOutput
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Guardrail.Scope != tc.scope || got.Guardrail.Status != tc.status {
				t.Fatalf("guardrail=%+v want scope=%s status=%s", got.Guardrail, tc.scope, tc.status)
			}
			if !tc.clean && got.DirtyDelta.Files != 1 {
				t.Fatalf("dirty delta lost: %+v", got.DirtyDelta)
			}
			if tc.full && (got.Guardrail.Files != got.Candidate.Files || got.Guardrail.Bytes != got.Candidate.Bytes) {
				t.Fatalf("full guardrail=%+v candidate=%+v", got.Guardrail, got.Candidate)
			}
			if tc.full {
				cfg, err := loadConfig()
				if err != nil {
					t.Fatal(err)
				}
				archive, err := PrepareDelegatedArchive(context.Background(), DelegatedArchivePreparationRequest{Config: cfg, Repo: Repo{Root: dir}})
				if archive != nil {
					defer archive.Close()
				}
				if (err != nil) != (got.Guardrail.Status == "failed") {
					t.Fatalf("preview=%+v archive preflight=%v", got.Guardrail, err)
				}
			}
			if provider.configured != 0 || provider.warmed != 0 || provider.ran != 0 {
				t.Fatalf("local preview configured or used provider: %+v", provider)
			}
		})
	}
}

func TestSyncPlanAnnotatesProtectedTrackedFilesWithBoundedExamples(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_DEFAULT_CLASS", "")

	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	for i := 0; i < 7; i++ {
		writeFile(t, filepath.Join(dir, "target", fmt.Sprintf("file-%d.txt", i)), "tracked\n")
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"sync-plan"}); err != nil {
		t.Fatalf("sync-plan: %v stderr=%q", err, stderr.String())
	}
	annotation := "warning: protected 7 tracked files from built-in artifact excludes:"
	if !strings.Contains(stdout.String(), annotation) || !strings.Contains(stdout.String(), "target/file-0.txt (target)") || !strings.Contains(stdout.String(), "(+2 more)") {
		t.Fatalf("sync-plan output missing bounded protection annotation:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := app.Run(context.Background(), []string{"sync-plan", "--json"}); err != nil {
		t.Fatalf("sync-plan --json: %v stderr=%q", err, stderr.String())
	}
	var got syncPlanJSONOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode sync-plan JSON: %v\n%s", err, stdout.String())
	}
	if got.ProtectedTracked.Count != 7 || len(got.ProtectedTracked.Examples) != protectedTrackedExcludeExampleLimit {
		t.Fatalf("protectedTrackedFiles=%+v", got.ProtectedTracked)
	}
}

func syncPlanHasReason(got []syncPlanJSONGuardrailReason, want syncPlanJSONGuardrailReason) bool {
	for _, reason := range got {
		if reason == want {
			return true
		}
	}
	return false
}
