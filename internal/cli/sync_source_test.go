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
