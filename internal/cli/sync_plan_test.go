package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

func syncPlanHasReason(got []syncPlanJSONGuardrailReason, want syncPlanJSONGuardrailReason) bool {
	for _, reason := range got {
		if reason == want {
			return true
		}
	}
	return false
}
