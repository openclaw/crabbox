package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func localReceiverFixture(t *testing.T, objectFormats ...string) (gitLocalSeedPlan, []byte, string) {
	t.Helper()
	root := t.TempDir()
	format := "sha1"
	if len(objectFormats) != 0 {
		format = objectFormats[0]
	}
	runGit(t, root, "init", "-q", "--object-format="+format)
	runGit(t, root, "config", "user.name", "Alice")
	runGit(t, root, "config", "user.email", "alice@example.com")
	mustWriteTestFile(t, filepath.Join(root, "historical.txt"), "complete historical blob\n")
	mustWriteTestFile(t, filepath.Join(root, "accepted.txt"), "committed content\n")
	mustWriteTestFile(t, filepath.Join(root, "excluded.txt"), "not a materialized path\n")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "base")
	runGit(t, root, "tag", "-a", "v1.0.0", "-m", "version one")
	base := gitOutput(root, "rev-parse", "HEAD")
	runGit(t, root, "rm", "-q", "historical.txt")
	runGit(t, root, "commit", "-qm", "current")
	head := gitOutput(root, "rev-parse", "HEAD")
	runGit(t, root, "update-ref", "refs/crabbox/local-head", head)
	runGit(t, root, "update-ref", "refs/heads/base", base)
	refs := []localGitSeedRef{
		{Name: "refs/crabbox/local-head", OID: head},
		{Name: "refs/heads/base", OID: base},
		{Name: "refs/tags/v1.0.0", OID: gitOutput(root, "rev-parse", "refs/tags/v1.0.0")},
	}
	bundle := filepath.Join(t.TempDir(), "seed.bundle")
	runGit(t, root, "bundle", "create", bundle, "refs/crabbox/local-head", "refs/heads/base", "refs/tags/v1.0.0")
	data, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return gitLocalSeedPlan{
		Head: head, Tree: gitOutput(root, "rev-parse", "HEAD^{tree}"), ObjectFormat: format,
		Digest: hex.EncodeToString(digest[:]), Refs: refs, PackedBytes: int64(len(data)), Fingerprint: strings.Repeat("b", 64),
	}, data, root
}

func TestGitLocalReceiverSHA256(t *testing.T) {
	plan, data, _ := localReceiverFixture(t, "sha256")
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	requireLocalReceiver(t, remoteGitLocalSeedFinalize(workdir, plan), nil)
	if got := gitOutput(workdir, "rev-parse", "--show-object-format"); got != "sha256" {
		t.Fatalf("object format changed: %q", got)
	}
	if got := gitOutput(workdir, "show", "HEAD^:historical.txt"); got != "complete historical blob" {
		t.Fatalf("SHA256 historical closure: %q", got)
	}
}

func TestGitLocalReceiverDetachedNoBase(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			plan, _, source := localReceiverFixture(t, format)
			bundle := filepath.Join(t.TempDir(), "head.bundle")
			runGit(t, source, "bundle", "create", bundle, "refs/crabbox/local-head")
			data, err := os.ReadFile(bundle)
			if err != nil {
				t.Fatal(err)
			}
			plan.Refs = plan.Refs[:1]
			plan.PackedBytes = int64(len(data))
			digest := sha256.Sum256(data)
			plan.Digest = hex.EncodeToString(digest[:])
			workdir := t.TempDir()
			requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
			requireLocalReceiver(t, remoteGitLocalSeedFinalize(workdir, plan), nil)
			if refs := gitOutput(workdir, "for-each-ref", "--format=%(refname)"); refs != "" {
				t.Fatalf("detached no-base receiver invented refs: %q", refs)
			}
			if head := gitOutput(workdir, "rev-parse", "HEAD"); head != plan.Head {
				t.Fatalf("detached HEAD changed: %q", head)
			}
			if out := requireLocalReceiver(t, remoteGitLocalSeedFingerprint(workdir, plan), nil); string(out) != plan.Fingerprint {
				t.Fatalf("detached no-base fingerprint: %q", out)
			}
		})
	}
}

func runLocalReceiver(t *testing.T, command string, input []byte) ([]byte, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX receiver execution; native Windows has a separate platform gate")
	}
	cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", command)
	cmd.Stdin = bytes.NewReader(input)
	return cmd.CombinedOutput()
}

func requireLocalReceiver(t *testing.T, command string, input []byte) []byte {
	t.Helper()
	out, err := runLocalReceiver(t, command, input)
	if err != nil {
		t.Fatalf("local receiver: %v\n%s", err, out)
	}
	return out
}

func TestGitLocalReceiverMetadataOnlyAndFinalize(t *testing.T) {
	plan, data, source := localReceiverFixture(t)
	workdir := t.TempDir()
	mustWriteTestFile(t, filepath.Join(workdir, "accepted.txt"), "accepted dirty content\n")
	mustWriteTestFile(t, filepath.Join(workdir, "untracked.txt"), "accepted untracked\n")
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	for _, name := range []string{"excluded.txt", "historical.txt", ".git/crabbox-local-complete", ".git/crabbox/sync-fingerprint"} {
		if _, err := os.Stat(filepath.Join(workdir, name)); !os.IsNotExist(err) {
			t.Errorf("unexpected materialization %s: %v", name, err)
		}
	}
	if content, err := os.ReadFile(filepath.Join(workdir, "accepted.txt")); err != nil || string(content) != "accepted dirty content\n" {
		t.Fatalf("accepted dirty path changed: %q %v", content, err)
	}
	if gitOutput(workdir, "rev-parse", "HEAD") != plan.Head || gitOutput(workdir, "write-tree") != plan.Tree {
		t.Fatal("HEAD/index identity mismatch")
	}
	if refs := gitOutput(workdir, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/base\nrefs/tags/v1.0.0" {
		t.Fatalf("receiver must retain only selected user refs: %q", refs)
	}
	if got := gitOutput(workdir, "show", "HEAD^:historical.txt"); got != "complete historical blob" {
		t.Fatalf("historical closure: %q", got)
	}
	if got := gitOutput(workdir, "describe", "--abbrev=0", "HEAD"); got != gitOutput(source, "describe", "--abbrev=0", "HEAD") {
		t.Fatalf("selected tag identity: %q", got)
	}
	if out, err := runLocalReceiver(t, remoteGitLocalSeedFingerprint(workdir, plan), nil); err == nil || bytes.Contains(out, []byte(plan.Fingerprint)) {
		t.Fatalf("unfinalized metadata returned a fingerprint: %q %v", out, err)
	}
	requireLocalReceiver(t, remoteGitLocalSeedFinalize(workdir, plan), nil)
	if out := requireLocalReceiver(t, remoteGitLocalSeedFingerprint(workdir, plan), nil); string(out) != plan.Fingerprint {
		t.Fatalf("fingerprint: %q", out)
	}
	if err := os.Remove(filepath.Join(workdir, ".git/crabbox/sync-fingerprint")); err != nil {
		t.Fatal(err)
	}
	if out, err := runLocalReceiver(t, remoteGitLocalSeedFingerprint(workdir, plan), nil); err == nil || bytes.Contains(out, []byte(plan.Fingerprint)) {
		t.Fatalf("workload-invalidated fingerprint reused: %q %v", out, err)
	}
}

func TestGitLocalReceiverRawManifestHandoffPrunesPriorFiles(t *testing.T) {
	plan, data, _ := localReceiverFixture(t)
	workdir := t.TempDir()
	rawToken, localToken := strings.Repeat("a", 32), strings.Repeat("b", 32)
	oldPath := "old generated\nfile.txt"
	unmanagedPath := "excluded.txt"
	mustWriteTestFile(t, filepath.Join(workdir, oldPath), "previously synced\n")
	mustWriteTestFile(t, filepath.Join(workdir, unmanagedPath), "runner-owned content\n")
	rawManifest := []byte(oldPath + "\x00accepted.txt\x00")
	requireLocalReceiver(t, remoteWriteSyncManifestsNewMode(workdir, rawToken, true),
		[]byte(syncManifestInputForTarget(SSHTarget{}, rawManifest, nil)))
	mustWriteTestFile(t, filepath.Join(workdir, "accepted.txt"), "old content\n")
	requireLocalReceiver(t, remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: rawToken, PlainManifest: true}), nil)
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox/sync-fingerprint"), "stale fingerprint")
	mustWriteTestFile(t, filepath.Join(workdir, ".crabbox/git-hydrate-base"), "stale base")

	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	if got, err := os.ReadFile(filepath.Join(workdir, ".git/crabbox/sync-manifest")); err != nil || !bytes.Equal(got, rawManifest) {
		t.Fatalf("raw manifest handoff: %q %v", got, err)
	}
	for _, name := range []string{"crabbox-local-complete", "crabbox/sync-fingerprint", "crabbox/git-hydrate-base"} {
		if _, err := os.Stat(filepath.Join(workdir, ".git", name)); !os.IsNotExist(err) {
			t.Fatalf("raw readiness marker imported: %s %v", name, err)
		}
	}
	newManifest := []byte("accepted.txt\x00")
	requireLocalReceiver(t, remoteWriteSyncManifestsNew(workdir, localToken),
		[]byte(syncManifestInputForTarget(SSHTarget{}, newManifest, nil)))
	requireLocalReceiver(t, remotePruneSyncManifestForTargetMode(SSHTarget{}, workdir, localToken, true), nil)
	mustWriteTestFile(t, filepath.Join(workdir, "accepted.txt"), "new content\n")
	requireLocalReceiver(t, remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: localToken}), nil)
	requireLocalReceiver(t, remoteGitLocalSeedFinalize(workdir, plan), nil)

	if _, err := os.Stat(filepath.Join(workdir, oldPath)); !os.IsNotExist(err) {
		t.Fatalf("previously managed file survived prune: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workdir, unmanagedPath)); err != nil || string(got) != "runner-owned content\n" {
		t.Fatalf("unmanaged file changed: %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(workdir, ".git/crabbox/sync-manifest")); err != nil || !bytes.Equal(got, newManifest) {
		t.Fatalf("new committed manifest: %q %v", got, err)
	}
	if gitOutput(workdir, "rev-parse", "HEAD") != plan.Head || gitOutput(workdir, "write-tree") != plan.Tree {
		t.Fatal("local metadata identity changed")
	}
}

func TestGitLocalReceiverPruneRejectsSymlinkAncestors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX and WSL shell pruning behavior")
	}
	plan, data, _ := localReceiverFixture(t)
	for _, mode := range []string{"posix", "wsl2"} {
		t.Run(mode, func(t *testing.T) {
			target := SSHTarget{}
			if mode == "wsl2" {
				target = SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
			}
			for _, attachment := range []string{"raw", "owned-reuse"} {
				t.Run(attachment, func(t *testing.T) {
					workdir := t.TempDir()
					const oldPath = "generated/old.txt"
					const rawToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
					const token = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
					mustWriteTestFile(t, filepath.Join(workdir, oldPath), "prior synced file\n")
					requireLocalReceiver(t, remoteWriteSyncManifestsNewMode(workdir, rawToken, true),
						[]byte(syncManifestInputForTarget(SSHTarget{}, []byte(oldPath+"\x00"), nil)))
					requireLocalReceiver(t, remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{Token: rawToken, PlainManifest: true}), nil)
					if attachment == "owned-reuse" {
						requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
					}
					outside := t.TempDir()
					protected := filepath.Join(outside, "old.txt")
					mustWriteTestFile(t, protected, "outside workspace\n")
					if err := os.RemoveAll(filepath.Join(workdir, "generated")); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, filepath.Join(workdir, "generated")); err != nil {
						t.Fatal(err)
					}
					requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
					requireLocalReceiver(t, remoteWriteSyncManifestsNewForTarget(target, workdir, token),
						[]byte(syncManifestInputForTarget(target, []byte("accepted.txt\x00"), nil)))
					out, err := runLocalReceiver(t, remotePruneSyncManifestForTargetMode(target, workdir, token, true), nil)
					if got, readErr := os.ReadFile(protected); readErr != nil || string(got) != "outside workspace\n" {
						t.Fatalf("prune escaped workspace: %q %v", got, readErr)
					}
					if err == nil || !bytes.Contains(out, []byte("refuses symlink ancestor")) {
						t.Fatalf("unsafe prune was not rejected: %q %v", out, err)
					}
					if gitOutput(workdir, "rev-parse", "HEAD") != plan.Head || gitOutput(workdir, "write-tree") != plan.Tree {
						t.Fatal("rejected prune changed Git identity")
					}
				})
			}
		})
	}
}

func TestGitLocalReceiverRejectsInvalidRawManifestBeforePublication(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX receiver behavior")
	}
	plan, data, _ := localReceiverFixture(t)
	for _, scenario := range []string{"directory", "symlink", "dangling-symlink", "parent-symlink"} {
		t.Run(scenario, func(t *testing.T) {
			workdir := t.TempDir()
			manifest := filepath.Join(workdir, ".crabbox/sync-manifest")
			mustWriteTestFile(t, filepath.Join(workdir, "owned.txt"), "raw content")
			if scenario != "parent-symlink" {
				if err := os.MkdirAll(filepath.Dir(manifest), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "parent-symlink" {
				source := t.TempDir()
				mustWriteTestFile(t, filepath.Join(source, "sync-manifest"), "owned.txt\x00")
				if err := os.Symlink(source, filepath.Dir(manifest)); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "directory" {
				if err := os.Mkdir(manifest, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				source := filepath.Join(t.TempDir(), "manifest")
				if scenario == "symlink" {
					mustWriteTestFile(t, source, "owned.txt\x00")
				}
				if err := os.Symlink(source, manifest); err != nil {
					t.Fatal(err)
				}
			}
			out, err := runLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
			if err == nil || !bytes.Contains(out, []byte("publish failed")) {
				t.Fatalf("invalid raw manifest accepted: %q %v", out, err)
			}
			if _, err := os.Lstat(filepath.Join(workdir, ".git")); !os.IsNotExist(err) {
				t.Fatalf("failed import published metadata: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(workdir, "owned.txt")); err != nil || string(got) != "raw content" {
				t.Fatalf("failed import changed raw file: %q %v", got, err)
			}
			if entries, err := filepath.Glob(filepath.Join(workdir, ".crabbox-local-git.*")); err != nil || len(entries) != 0 {
				t.Fatalf("failed import staging residue: %v %v", entries, err)
			}
		})
	}
}

func TestGitLocalReceiverManifestSelection(t *testing.T) {
	testGitLocalReceiverManifestSelection(t, func(t *testing.T, command string, input []byte) {
		requireLocalReceiver(t, command, input)
	}, remoteGitLocalSeed)
}

func testGitLocalReceiverManifestSelection(t *testing.T, receive func(*testing.T, string, []byte), command func(string, gitLocalSeedPlan) string) {
	t.Helper()
	plan, data, _ := localReceiverFixture(t)
	for _, scenario := range []string{"raw", "raw-absent", "raw-empty", "owned", "owned-absent"} {
		t.Run(scenario, func(t *testing.T) {
			workdir := t.TempDir()
			raw := filepath.Join(workdir, ".crabbox/sync-manifest")
			canonical := filepath.Join(workdir, ".git/crabbox/sync-manifest")
			want := "raw old.txt\x00"
			if strings.HasPrefix(scenario, "owned") {
				receive(t, command(workdir, plan), data)
				if scenario == "owned" {
					want = "canonical old.txt\x00"
					mustWriteTestFile(t, canonical, want)
				}
			}
			if scenario != "raw-absent" {
				rawContent := "raw old.txt\x00"
				if scenario == "raw-empty" {
					rawContent, want = "", ""
				}
				mustWriteTestFile(t, raw, rawContent)
				mustWriteTestFile(t, filepath.Join(workdir, ".crabbox/sync-fingerprint"), "stale fingerprint")
				mustWriteTestFile(t, filepath.Join(workdir, ".crabbox/git-hydrate-base"), "stale base")
			}
			receive(t, command(workdir, plan), data)
			got, err := os.ReadFile(canonical)
			if strings.HasSuffix(scenario, "-absent") {
				if !os.IsNotExist(err) {
					t.Fatalf("invented prior ownership: %q %v", got, err)
				}
			} else if err != nil || string(got) != want {
				t.Fatalf("wrong prior manifest: %q %v; want %q", got, err, want)
			}
			for _, name := range []string{"crabbox-local-complete", "crabbox/sync-fingerprint", "crabbox/git-hydrate-base"} {
				if _, err := os.Stat(filepath.Join(workdir, ".git", name)); !os.IsNotExist(err) {
					t.Fatalf("stale readiness marker imported: %s %v", name, err)
				}
			}
		})
	}
}

func TestGitLocalReceiverWindowsManifestHandoff(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows receiver behavior")
	}
	shell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal(err)
	}
	receive := func(command string, input []byte) ([]byte, error) {
		script := filepath.Join(t.TempDir(), "receiver.ps1")
		// Keep production errors private while exposing native failures in this fixture.
		body := strings.Replace(decodePowerShellCommand(t, command),
			`[Console]::Error.WriteLine("local Git seed: $phase failed")`,
			`[Console]::Error.WriteLine($_.Exception.GetType().FullName + ": " + $_.Exception.Message)
  [Console]::Error.WriteLine("local Git seed: $phase failed")`, 1)
		mustWriteTestFile(t, script, body)
		cmd := exec.Command(shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
		// The CI host starts Go from PowerShell 7, whose module paths break 5.1
		// cmdlet discovery through intermediary processes. Let 5.1 build its own paths.
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			if !strings.EqualFold(name, "PSModulePath") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Stdin = bytes.NewReader(input)
		return cmd.CombinedOutput()
	}
	testGitLocalReceiverManifestSelection(t, func(t *testing.T, command string, input []byte) {
		if out, err := receive(command, input); err != nil {
			t.Fatalf("native Windows receiver: %v\n%s", err, out)
		}
	}, windowsGitLocalSeed)
	for _, scenario := range []string{"directory", "parent-junction"} {
		t.Run(scenario, func(t *testing.T) {
			plan, data, _ := localReceiverFixture(t)
			workdir := t.TempDir()
			if scenario == "directory" {
				if err := os.MkdirAll(filepath.Join(workdir, ".crabbox/sync-manifest"), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				source := t.TempDir()
				mustWriteTestFile(t, filepath.Join(source, "sync-manifest"), "owned.txt\x00")
				if out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", filepath.Join(workdir, ".crabbox"), source).CombinedOutput(); err != nil {
					t.Fatalf("create fixture junction: %v\n%s", err, out)
				}
			}
			mustWriteTestFile(t, filepath.Join(workdir, "owned.txt"), "raw content")
			out, err := receive(windowsGitLocalSeed(workdir, plan), data)
			if err == nil || !bytes.Contains(out, []byte("publish failed")) {
				t.Fatalf("invalid raw manifest accepted: %q %v", out, err)
			}
			if _, err := os.Lstat(filepath.Join(workdir, ".git")); !os.IsNotExist(err) {
				t.Fatalf("failed import published metadata: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(workdir, "owned.txt")); err != nil || string(got) != "raw content" {
				t.Fatalf("failed import changed raw file: %q %v", got, err)
			}
		})
	}
}

func TestGitLocalReceiverOwnedReusePreservesManifestOnly(t *testing.T) {
	plan, data, _ := localReceiverFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	requireLocalReceiver(t, remoteGitLocalSeedFinalize(workdir, plan), nil)
	mustWriteTestFile(t, filepath.Join(workdir, ".git/crabbox/sync-manifest"), "old.txt\x00")
	mustWriteTestFile(t, filepath.Join(workdir, ".git/crabbox/git-hydrate-base"), "old base")
	mustWriteTestFile(t, filepath.Join(workdir, "old.txt"), "prior working file")
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	if got, err := os.ReadFile(filepath.Join(workdir, ".git/crabbox/sync-manifest")); err != nil || string(got) != "old.txt\x00" {
		t.Fatalf("prior manifest lost: %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(workdir, "old.txt")); err != nil || string(got) != "prior working file" {
		t.Fatalf("working file changed: %q %v", got, err)
	}
	for _, name := range []string{"crabbox-local-complete", "crabbox/sync-fingerprint", "crabbox/git-hydrate-base"} {
		if _, err := os.Stat(filepath.Join(workdir, ".git", name)); !os.IsNotExist(err) {
			t.Errorf("old marker survived: %s %v", name, err)
		}
	}
	entries, err := filepath.Glob(filepath.Join(workdir, ".crabbox-local-git.*"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging residue: %v %v", entries, err)
	}
}

func TestGitLocalReceiverRejectsInvalidArtifactBeforePublishing(t *testing.T) {
	plan, data, _ := localReceiverFixture(t)
	for _, scenario := range []string{"digest", "head", "tree", "refs", "truncated"} {
		t.Run(scenario, func(t *testing.T) {
			candidate := plan
			payload := data
			switch scenario {
			case "digest":
				candidate.Digest = strings.Repeat("0", 64)
			case "head":
				candidate.Head = candidate.Refs[1].OID
			case "tree":
				candidate.Tree = strings.Repeat("0", 40)
			case "refs":
				candidate.Refs = candidate.Refs[:1]
			case "truncated":
				payload = data[:len(data)-5]
			}
			workdir := t.TempDir()
			out, err := runLocalReceiver(t, remoteGitLocalSeed(workdir, candidate), payload)
			if err == nil || !bytes.Contains(out, []byte("local Git seed:")) {
				t.Fatalf("invalid artifact accepted: %q %v", out, err)
			}
			entries, err := os.ReadDir(workdir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed receiver published metadata/residue: %v %v", entries, err)
			}
		})
	}
}

func TestGitLocalReceiverPreservesUnownedRepository(t *testing.T) {
	plan, data, _ := localReceiverFixture(t)
	workdir := t.TempDir()
	runGit(t, workdir, "init", "-q")
	head, err := os.ReadFile(filepath.Join(workdir, ".git/HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data); err == nil || !bytes.Contains(out, []byte("ownership failed")) {
		t.Fatalf("unowned repository accepted: %q %v", out, err)
	} else if !bytes.Contains(out, []byte("Use --full-resync only if you intend to replace this workspace.")) {
		t.Fatalf("ownership refusal lacks replacement guidance: %q", out)
	}
	got, err := os.ReadFile(filepath.Join(workdir, ".git/HEAD"))
	if err != nil || !bytes.Equal(got, head) {
		t.Fatalf("unowned HEAD changed: %q %v", got, err)
	}
}

func TestGitLocalReceiverFinalizeFailureClearsPreviousSuccess(t *testing.T) {
	plan, data, _ := localReceiverFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	requireLocalReceiver(t, remoteGitLocalSeedFinalize(workdir, plan), nil)
	runGit(t, workdir, "read-tree", "HEAD^")
	if out, err := runLocalReceiver(t, remoteGitLocalSeedFinalize(workdir, plan), nil); err == nil || !bytes.Contains(out, []byte("verify-metadata failed")) {
		t.Fatalf("changed index finalized: %q %v", out, err)
	}
	for _, name := range []string{"crabbox-local-complete", "crabbox/sync-fingerprint"} {
		if _, err := os.Stat(filepath.Join(workdir, ".git", name)); !os.IsNotExist(err) {
			t.Errorf("failed finalization retained success: %s %v", name, err)
		}
	}
}

func TestGitLocalReceiverWindowsCommandGeneration(t *testing.T) {
	plan, _, _ := localReceiverFixture(t)
	importScript := decodePowerShellCommand(t, windowsGitLocalSeed(`C:\work\repo`, plan))
	finalScript := decodePowerShellCommand(t, windowsGitLocalSeedFinalize(`C:\work\repo`, plan))
	for _, fragment := range []string{"OpenStandardInput", "$stdin.CopyTo($payload)", "SetAccessRuleProtection", "Get-FileHash", "bundle unbundle", "read-tree $expectedHead", "Assert-LocalMetadata", "retained previous metadata"} {
		if !strings.Contains(importScript, fragment) {
			t.Errorf("missing generated Windows operation %q", fragment)
		}
	}
	if !strings.Contains(finalScript, "$finalizing = $true") || strings.Contains(finalScript, "bundle unbundle") {
		t.Fatal("Windows finalize does not have a separate verification phase")
	}
	if !strings.Contains(importScript, "Use --full-resync only if you intend to replace this workspace.") {
		t.Fatal("Windows ownership refusal lacks replacement guidance")
	}
	if shell, err := exec.LookPath("pwsh"); err == nil {
		for _, script := range []string{importScript, finalScript} {
			path := filepath.Join(t.TempDir(), "receiver.ps1")
			mustWriteTestFile(t, path, script)
			check := `$tokens=$null; $errors=$null; $null=[System.Management.Automation.Language.Parser]::ParseFile($args[0],[ref]$tokens,[ref]$errors); if ($errors.Count) { $errors | ForEach-Object { $_.ToString() }; exit 1 }`
			checker := filepath.Join(t.TempDir(), "parse.ps1")
			mustWriteTestFile(t, checker, check)
			if out, err := exec.Command(shell, "-NoLogo", "-NoProfile", "-File", checker, path).CombinedOutput(); err != nil {
				t.Fatalf("generated Windows command syntax: %v\n%s", err, out)
			}
		}
	}
}

func TestGitLocalReceiverWindowsRedirectedBinaryInput(t *testing.T) {
	shell, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell is unavailable for redirected stream verification")
	}
	plan, data, _ := localReceiverFixture(t)
	script := decodePowerShellCommand(t, windowsGitLocalSeed("repo", plan))
	start := strings.Index(script, "  $payload = [IO.File]::Open(")
	end := strings.Index(script, "  $phase = 'digest'")
	if start < 0 || end < start {
		t.Fatal("missing receiver input block")
	}
	for _, scenario := range []string{"exact", "short", "extra"} {
		t.Run(scenario, func(t *testing.T) {
			input := data
			switch scenario {
			case "short":
				input = data[:len(data)-1]
			case "extra":
				input = append(append([]byte{}, data...), 0)
			}
			bundle := filepath.Join(t.TempDir(), "payload.bundle")
			checker := filepath.Join(t.TempDir(), "receive.ps1")
			mustWriteTestFile(t, checker, "$ErrorActionPreference = 'Stop'\n$bundle = "+psQuote(bundle)+"\n"+script[start:end])
			command := exec.Command(shell, "-NoLogo", "-NoProfile", "-File", checker)
			command.Stdin = bytes.NewReader(input)
			out, err := command.CombinedOutput()
			if scenario == "exact" {
				if err != nil {
					t.Fatalf("redirected binary input failed: %v\n%s", err, out)
				}
				got, err := os.ReadFile(bundle)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("binary payload changed: %v", err)
				}
			} else if err == nil || !bytes.Contains(out, []byte("payload length mismatch")) {
				t.Fatalf("incorrect payload length accepted: %v\n%s", err, out)
			}
		})
	}
}

func TestGitLocalReceiverCommandSizeIndependentOfRefs(t *testing.T) {
	plan, _, _ := localReceiverFixture(t)
	one := plan
	one.Refs = plan.Refs[:1]
	several := plan
	several.Refs = append(append([]localGitSeedRef{}, plan.Refs...), localGitSeedRef{Name: "refs/tags/another-release", OID: plan.Head})
	for name, command := range map[string]func(string, gitLocalSeedPlan) string{
		"import": remoteGitLocalSeed, "finalize": remoteGitLocalSeedFinalize, "fingerprint": remoteGitLocalSeedFingerprint,
		"windows-import": windowsGitLocalSeed, "windows-finalize": windowsGitLocalSeedFinalize,
	} {
		t.Run(name, func(t *testing.T) {
			if single, multiple := len(command("repo", one)), len(command("repo", several)); single != multiple {
				t.Fatalf("command size depends on refs: one=%d several=%d", single, multiple)
			}
		})
	}
}

func TestGitLocalReceiverFingerprintChecksExactRefs(t *testing.T) {
	plan, data, _ := localReceiverFixture(t)
	workdir := t.TempDir()
	requireLocalReceiver(t, remoteGitLocalSeed(workdir, plan), data)
	requireLocalReceiver(t, remoteGitLocalSeedFinalize(workdir, plan), nil)
	runGit(t, workdir, "update-ref", "refs/tags/another-release", plan.Head)
	if out, err := runLocalReceiver(t, remoteGitLocalSeedFingerprint(workdir, plan), nil); err == nil || bytes.Contains(out, []byte(plan.Fingerprint)) {
		t.Fatalf("changed refs reused fingerprint: %q %v", out, err)
	}
}

func TestGitLocalReceiverWindowsRefDigestCanonicalization(t *testing.T) {
	shell, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell is unavailable for portable ref-hashing helper verification")
	}
	plan, _, _ := localReceiverFixture(t)
	plan.Refs = append(plan.Refs,
		localGitSeedRef{Name: "refs/tags/café", OID: plan.Head},
		localGitSeedRef{Name: "refs/tags/release-Ｚ", OID: plan.Head},
		localGitSeedRef{Name: "refs/tags/release-🚀", OID: plan.Head},
	)
	script := decodePowerShellCommand(t, windowsGitLocalSeed("repo", plan))
	start := strings.Index(script, "function Sort-LocalLines(")
	end := strings.Index(script, "function Invoke-LocalGit {")
	if start < 0 || end < start {
		t.Fatal("missing ref hashing helpers")
	}
	check := script[start:end] + "\n$lines = @(\n"
	for _, ref := range plan.Refs {
		check += psQuote(ref.OID+" "+ref.Name) + "\n"
	}
	check += ")\n[Console]::Write((Get-LocalRefsDigest (Sort-LocalLines $lines)))\n"
	checker := filepath.Join(t.TempDir(), "ref-digest.ps1")
	mustWriteTestFile(t, checker, check)
	out, err := exec.Command(shell, "-NoLogo", "-NoProfile", "-File", checker).CombinedOutput()
	if err != nil || string(out) != plan.refsDigest() {
		t.Fatalf("PowerShell ref digest differs from canonical UTF-8: %q %v", out, err)
	}
}
