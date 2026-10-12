package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func installFakeJJ(t *testing.T, root, commit, bookmarks, flags string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake jj shell fixture requires POSIX")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *'op log'*) printf '%%s' '%s';;
  *'git root'*) printf '%%s' '%s';;
  *'bookmark list'*) printf '%%s' %s;;
  *'log --no-graph'*) printf '%%s\n%%s\n%%s' '%s' '%s' 'false';;
  *) exit 1;;
esac
`, strings.Repeat("a", 128), filepath.Join(root, ".git"), shellQuote(bookmarks), commit, flags)
	if err := os.WriteFile(filepath.Join(dir, "jj"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func fakeJJBookmark(name, commit string, conflict bool) string {
	value, _ := json.Marshal(map[string]any{"name": name, "remote": "origin", "commit": commit, "conflict": conflict})
	return string(value)
}

func jjFixtureGit(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init")
	writeFile(t, filepath.Join(root, "base.txt"), "base\n")
	writeFile(t, filepath.Join(root, "deleted.txt"), "remove me\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "base")
	base := gitOutput(root, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(root, "base.txt"), "working-copy edit\n")
	writeFile(t, filepath.Join(root, "a.txt"), "first parent\n")
	writeFile(t, filepath.Join(root, "b.txt"), "second parent\n")
	writeFile(t, filepath.Join(root, "c.txt"), "third parent\n")
	runGit(t, root, "rm", "deleted.txt")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "working copy")
	return root, base, gitOutput(root, "rev-parse", "HEAD")
}

func TestJujutsuFakeSnapshotUsesExactCommitAndAdvertisedBase(t *testing.T) {
	store, base, commit := jjFixtureGit(t)
	workspace := t.TempDir()
	makeNativeJujutsuWorkspace(t, workspace)
	writeFile(t, filepath.Join(workspace, "wrong.txt"), "not the pinned tree\n")
	installFakeJJ(t, store, commit, fakeJJBookmark("main", base, false), "false")
	runGit(t, store, "remote", "add", "origin", "https://example.com/project.git")
	repo, source, err := prepareJujutsuSource(context.Background(), Repo{Root: workspace}, baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.cleanup(); err != nil {
			t.Error(err)
		}
	})
	manifest, err := syncManifest(repo.Root, configuredExcludes(baseConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.txt", "b.txt", "base.txt", "c.txt"}; !reflect.DeepEqual(manifest.Files, want) {
		t.Fatalf("files=%v want %v", manifest.Files, want)
	}
	if !reflect.DeepEqual(manifest.Deleted, []string{"deleted.txt"}) {
		t.Fatalf("deleted=%v", manifest.Deleted)
	}
	if repo.Head != base || repo.BaseRef != "origin/main" || source.Commit != commit {
		t.Fatalf("repo=%+v commit=%s", repo, source.Commit)
	}
	got, err := os.ReadFile(filepath.Join(repo.Root, "base.txt"))
	if err != nil || string(got) != "working-copy edit\n" {
		t.Fatalf("content=%q err=%v", got, err)
	}
	plan, blocked := syncGitCoherencePlan(baseConfig(), repo)
	if blocked || !plan.enabled() || plan.Target != base || plan.Branch != "main" {
		t.Fatalf("plan=%+v blocked=%v", plan, blocked)
	}
}

func TestJujutsuFakeRejectsConflictAndBadBookmarks(t *testing.T) {
	store, base, commit := jjFixtureGit(t)
	for _, tc := range []struct{ name, flag, bookmarks string }{
		{"conflicted working copy", "true", ""},
		{"conflicted origin bookmark", "false", fakeJJBookmark("main", base, true)},
		{"invalid target", "false", fakeJJBookmark("main", "oops", false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			makeNativeJujutsuWorkspace(t, workspace)
			installFakeJJ(t, store, commit, tc.bookmarks, tc.flag)
			_, _, err := prepareJujutsuSource(context.Background(), Repo{Root: workspace}, baseConfig())
			if err == nil {
				t.Fatal("accepted ambiguous source")
			}
		})
	}
}

func TestJujutsuFakeWithoutOriginDoesNotSeed(t *testing.T) {
	store, _, commit := jjFixtureGit(t)
	workspace := t.TempDir()
	makeNativeJujutsuWorkspace(t, workspace)
	installFakeJJ(t, store, commit, "", "false")
	repo, source, err := prepareJujutsuSource(context.Background(), Repo{Root: workspace}, baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer source.cleanup()
	plan, _ := syncGitCoherencePlan(baseConfig(), repo)
	if plan.seedEnabled() || repo.Head != commit {
		t.Fatalf("plan=%+v repo=%+v", plan, repo)
	}
}

func TestJujutsuFakeFailsClosed(t *testing.T) {
	store, _, commit := jjFixtureGit(t)
	for _, kind := range []string{"missing binary", "divergent operations", "unsupported backend", "divergent change", "unknown base"} {
		t.Run(kind, func(t *testing.T) {
			workspace := t.TempDir()
			makeNativeJujutsuWorkspace(t, workspace)
			installFakeJJ(t, store, commit, "", "false")
			cfg := baseConfig()
			script, err := exec.LookPath("jj")
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(script)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing binary":
				t.Setenv("PATH", t.TempDir())
			case "divergent operations":
				data = []byte("#!/bin/sh\nexit 1\n")
			case "unsupported backend":
				data = []byte(strings.Replace(string(data), "*'git root'*)", "*'git root'*) exit 1;", 1))
			case "divergent change":
				data = []byte(strings.Replace(string(data), "'false' 'false'", "'false' 'true'", 1))
			case "unknown base":
				cfg.Sync.BaseRef = "origin/missing"
			}
			if err := os.WriteFile(script, data, 0755); err != nil {
				t.Fatal(err)
			}
			_, _, err = prepareJujutsuSource(context.Background(), Repo{Root: workspace}, cfg)
			if err == nil {
				t.Fatalf("accepted %s", kind)
			}
		})
	}
}

func TestJujutsuTreeMaterializationRejectsEscapingParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	runGit(t, root, "init")
	writeFile(t, filepath.Join(root, "dir", "payload"), "must stay in snapshot")
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "tree")
	commit := gitOutput(root, "rev-parse", "HEAD")
	if err := os.RemoveAll(filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := materializeJJTree(context.Background(), root, commit); err == nil {
		t.Fatal("accepted escaping parent")
	}
	if _, err := os.Stat(filepath.Join(outside, "payload")); !os.IsNotExist(err) {
		t.Fatalf("wrote outside snapshot: %v", err)
	}
}

func TestJujutsuRealOctopusWorkingCopy(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}
	root := t.TempDir()
	invoke := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("jj", append([]string{"--no-pager", "--color=never", "--config", "user.name=Fixture", "--config", "user.email=fixture@example.com"}, args...)...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("jj %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	invoke("git", "init", "--no-colocate", ".")
	writeFile(t, filepath.Join(root, "base.txt"), "base\n")
	base := invoke("log", "--no-graph", "-r", "@", "-T", "commit_id")
	store := invoke("git", "root")
	oddName := "dependabot/npm_and_yarn/@types/node-22"
	runGit(t, root, "--git-dir="+store, "remote", "add", "origin", "https://example.com/demo.git")
	for _, name := range []string{"main", oddName} {
		runGit(t, root, "--git-dir="+store, "update-ref", "refs/remotes/origin/"+name, base)
	}
	invoke("git", "import")
	invoke("bookmark", "track", "main@origin")
	var parents []string
	for _, name := range []string{"a", "b", "c"} {
		invoke("new", base)
		writeFile(t, filepath.Join(root, name+".txt"), name+"\n")
		parents = append(parents, invoke("log", "--no-graph", "-r", "@", "-T", "commit_id"))
	}
	invoke(append([]string{"new"}, parents...)...)
	writeFile(t, filepath.Join(root, "base.txt"), "uncommitted edit\n")
	writeFile(t, filepath.Join(root, "new.txt"), "uncommitted file\n")
	commit := invoke("log", "--no-graph", "-r", "@", "-T", "commit_id")
	invoke("bookmark", "set", "main", "-r", "@")
	if got := invoke("log", "--no-graph", "-r", "parents(@)", "-T", `commit_id ++ "\n"`); len(strings.Fields(got)) != 3 {
		t.Fatalf("not an octopus: %s", got)
	}
	cfg := baseConfig()
	cfg.Sync.BaseRef = "main"
	repo, source, err := prepareJujutsuSource(context.Background(), Repo{Root: root}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer source.cleanup()
	if repo.Head != base {
		t.Fatalf("seed used unpushed local bookmark: %s, want origin %s", repo.Head, base)
	}
	cfg.Sync.BaseRef = oddName
	oddRepo, oddSource, err := prepareJujutsuSource(context.Background(), Repo{Root: root}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer oddSource.cleanup()
	if oddRepo.BaseRef != "origin/"+oddName || oddRepo.Head != base {
		t.Fatalf("quoted bookmark mapping: %+v", oddRepo)
	}
	manifest, err := syncManifest(repo.Root, configuredExcludes(baseConfig()))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Fields(invoke("file", "list", "-r", "@"))
	if source.Commit != commit || !reflect.DeepEqual(manifest.Files, want) {
		t.Fatalf("commit=%s want=%s files=%v want=%v", source.Commit, commit, manifest.Files, want)
	}
	for _, path := range want {
		got, err := os.ReadFile(filepath.Join(repo.Root, path))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("jj", "--no-pager", "file", "show", "-r", "@", path)
		cmd.Dir = root
		expected, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, expected) {
			t.Fatalf("%s=%q want=%q", path, got, expected)
		}
	}
	clearConfigEnv(t)
	isolateRunTestUserDirs(t, t.TempDir())
	t.Chdir(root)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	var output bytes.Buffer
	if err := (App{Stdout: &output, Stderr: &output}).syncPlan(context.Background(), []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var plan syncPlanJSONOutput
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Source != "jj" || plan.Revision != commit || plan.Candidate.Files != len(want) {
		t.Fatalf("plan=%+v", plan)
	}
}
