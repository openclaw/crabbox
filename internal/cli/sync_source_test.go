package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirectorySyncNoSyncUsesCanonicalSourceRoot(t *testing.T) {
	clearConfigEnv(t)
	physical := t.TempDir()
	logical := filepath.Join(t.TempDir(), "source")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Chdir(logical)
	t.Setenv("PWD", logical)
	cfg := baseConfig()
	cfg.Sync.Source = "directory"

	repo, err := findSyncRepo(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Root != canonicalRepositoryPath(physical) || repo.Head != "" || repo.RemoteURL != "" {
		t.Fatalf("repo=%+v", repo)
	}
	existing := leaseClaim{LeaseID: "cbx_directory_alias", RepoRoot: canonicalRepositoryPath(physical)}
	if err := CheckLeaseClaimRepositoryOwner(existing.LeaseID, existing, repo.Root, false); err != nil {
		t.Fatalf("equivalent source refused: %v", err)
	}
	if err := CheckLeaseClaimRepositoryOwner(existing.LeaseID, existing, t.TempDir(), false); err == nil {
		t.Fatal("different source accepted")
	}
}

func TestCanonicalCurrentDirectoryRejectsRetargetedAlias(t *testing.T) {
	physical := t.TempDir()
	other := t.TempDir()
	logical := filepath.Join(t.TempDir(), "source")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Chdir(logical)
	t.Setenv("PWD", logical)
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(logical); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, logical); err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalCurrentDirectory(root); err == nil {
		t.Fatal("retargeted current-directory alias accepted")
	}
}

func TestDirectorySyncNoSyncPreservesGitMetadata(t *testing.T) {
	clearConfigEnv(t)
	physical := t.TempDir()
	runGit(t, physical, "init")
	writeFile(t, filepath.Join(physical, "README.txt"), "source\n")
	runGit(t, physical, "add", "README.txt")
	runGit(t, physical, "-c", "user.name=Test", "-c", "user.email=test@localhost", "commit", "-m", "initial")
	logical := filepath.Join(t.TempDir(), "source")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Chdir(logical)
	t.Setenv("PWD", logical)
	cfg := baseConfig()
	cfg.Sync.Source = "directory"

	repo, err := findSyncRepo(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Root != canonicalRepositoryPath(physical) || repo.Head == "" || repo.BaseRef == "" {
		t.Fatalf("repo=%+v", repo)
	}
}

func TestDirectoryClaimCompatibility(t *testing.T) {
	for _, command := range []string{"run", "exec"} {
		for _, tc := range []struct {
			name, owner, current string
			retarget, allowed    bool
		}{
			{"legacy-own-alias", "alias", "alias", false, true},
			{"legacy-physical-refused", "alias", "physical", false, false},
			{"legacy-other-alias-refused", "alias", "second-alias", false, false},
			{"physical-alias", "physical", "alias", false, true},
			{"physical-physical", "physical", "physical", false, true},
			{"different", "physical", "other", false, false},
			{"same-name-elsewhere", "physical", "same-name", false, false},
			{"retargeted", "physical", "alias", true, false},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				clearConfigEnv(t)
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				base := t.TempDir()
				physical := filepath.Join(base, "source")
				other := filepath.Join(base, "other")
				sameName := filepath.Join(base, "elsewhere", "source")
				for _, path := range []string{physical, other, sameName} {
					if err := os.MkdirAll(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				physical = canonicalRepositoryPath(physical)
				alias, secondAlias := filepath.Join(base, "alias"), filepath.Join(base, "second-alias")
				for _, path := range []string{alias, secondAlias} {
					if err := os.Symlink(physical, path); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				}
				paths := map[string]string{"physical": physical, "alias": alias, "second-alias": secondAlias, "other": other, "same-name": sameName}
				const id = "cbx_123456abcdef"
				// Released writers persist the logical path literally; use their actual writer and reader.
				if err := claimLeaseForRepo(id, "legacy", paths[tc.owner], 0, false); err != nil {
					t.Fatal(err)
				}
				claim, err := ReadLeaseClaim(id)
				if err != nil {
					t.Fatal(err)
				}
				if tc.retarget {
					if err := os.Remove(alias); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(other, alias); err != nil {
						t.Fatal(err)
					}
				}
				t.Chdir(paths[tc.current])
				t.Setenv("PWD", paths[tc.current])
				cfg := baseConfig()
				cfg.Sync.Source = "directory"
				var repo Repo
				if command == "run" {
					repo, err = findSyncRepo(cfg, false)
				} else {
					repo, err = findExecRepo(cfg)
				}
				if err != nil {
					t.Fatal(err)
				}
				err = CheckLeaseClaimRepositoryOwner(id, claim, repo.Root, false, repo.LegacyRoot)
				if (err == nil) != tc.allowed {
					t.Fatalf("allowed=%t, err=%v", tc.allowed, err)
				}
			})
		}
	}
}

func TestDirectoryClaimPublicationCanonicalizesLegacy(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	physical := canonicalRepositoryPath(t.TempDir())
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Chdir(alias)
	t.Setenv("PWD", alias)
	cfg := baseConfig()
	cfg.Sync.Source = "directory"
	repo, err := findSyncRepo(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg.claimLegacyRoot = repo.LegacyRoot
	for _, id := range []string{"cbx_123456abcde1", "cbx_123456abcde2"} {
		if id == "cbx_123456abcde1" {
			if err := claimLeaseForRepo(id, "legacy", alias, 0, false); err != nil {
				t.Fatal(err)
			}
		}
		if err := claimLeaseForRepoConfig(id, "fixture", cfg, repo.Root, 0, false); err != nil {
			t.Fatal(err)
		}
		claim, err := ReadLeaseClaim(id)
		if err != nil {
			t.Fatal(err)
		}
		if claim.RepoRoot != physical {
			t.Fatalf("stored root=%q want physical=%q", claim.RepoRoot, physical)
		}
	}
}

func TestDirectorySourceUnresolvableFailsClosed(t *testing.T) {
	for _, kind := range []string{"missing", "loop", "deleted-cwd"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "source")
			switch kind {
			case "loop":
				if err := os.Symlink(root, root); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "deleted-cwd":
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				t.Chdir(root)
				if err := os.Remove(root); err != nil {
					t.Skipf("cannot remove current directory: %v", err)
				}
			}
			if _, err := canonicalCurrentDirectory(root); err == nil {
				t.Fatal("unresolvable directory accepted")
			}
		})
	}
}

func TestGitSourceDiscoveryUnchanged(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	runGit(t, dir, "init")
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Chdir(alias)
	t.Setenv("PWD", alias)
	want, err := findRepo()
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		got, err := findSyncRepo(baseConfig(), enabled)
		if err != nil || got != want {
			t.Fatalf("enabled=%t got=%+v want=%+v err=%v", enabled, got, want, err)
		}
	}
}
