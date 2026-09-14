package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
)

// Local seeding freezes metadata and the ordinary complete manifest together;
// origin coherence and its optional file-only fallback are separate contracts.
type preparedLocalGitSeed struct {
	Selection localGitSeedSelection
	Artifact  localGitSeedArtifact
	Snapshot  gitOverlaySnapshot
	Plan      gitLocalSeedPlan
}

func (seed *preparedLocalGitSeed) cleanup() error {
	return errors.Join(seed.Artifact.cleanup(), seed.Snapshot.cleanup())
}

func selectLocalGitSeed(ctx context.Context, repo Repo, configuredBase string) (localGitSeedSelection, error) {
	selection := localGitSeedSelection{}
	head, err := localGitSeedSourceOutput(ctx, repo.Root, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil || !validGitObjectID(head) || (repo.Head != "" && repo.Head != head) {
		return selection, Exit(6, "local Git seed requires the unchanged committed HEAD")
	}
	tree, err := localGitSeedSourceOutput(ctx, repo.Root, "rev-parse", "--verify", "--end-of-options", head+"^{tree}")
	if err != nil || !validGitObjectID(tree) {
		return selection, Exit(6, "local Git seed cannot resolve the selected commit tree offline")
	}
	selection.Head, selection.Tree = head, tree
	base := strings.TrimSpace(configuredBase)
	explicit := base != ""
	if !explicit {
		base = strings.TrimSpace(repo.BaseRef)
	}
	if base == "" {
		return selection, nil
	}
	candidates := []string{base}
	if !strings.HasPrefix(base, "refs/") && !strings.HasPrefix(base, "origin/") && !validGitObjectID(base) {
		candidates = append([]string{"refs/remotes/origin/" + base}, candidates...)
	}
	for _, candidate := range candidates {
		oid, resolveErr := localGitSeedSourceOutput(ctx, repo.Root, "rev-parse", "--verify", "--end-of-options", candidate+"^{commit}")
		if resolveErr != nil || !validGitObjectID(oid) {
			continue
		}
		ref, refErr := localGitSeedSourceOutput(ctx, repo.Root, "rev-parse", "--symbolic-full-name", "--verify", "--end-of-options", candidate)
		if refErr != nil || !strings.HasPrefix(ref, "refs/") {
			ref = "refs/crabbox/local-base"
		}
		selection.Base, selection.BaseRef = oid, ref
		return selection, nil
	}
	if explicit {
		return selection, Exit(6, "local Git seed cannot resolve sync.baseRef locally; choose an available commit or ref")
	}
	return selection, nil
}

func prepareLocalGitSeed(ctx context.Context, repo Repo, cfg Config, force bool, stderr io.Writer) (seed preparedLocalGitSeed, result error) {
	ctx, cancel := context.WithTimeout(ctx, localGitSeedTimeout)
	defer cancel()
	defer func() {
		if result != nil {
			result = errors.Join(result, seed.cleanup())
		}
	}()
	selection, err := selectLocalGitSeed(ctx, repo, cfg.Sync.BaseRef)
	if err != nil {
		return seed, err
	}
	repo.Head = selection.Head
	excludes, err := syncExcludes(repo.Root, cfg)
	if err != nil {
		return seed, err
	}
	seed.Snapshot, err = prepareLocalGitSeedSnapshot(ctx, repo, cfg, excludes)
	if err != nil {
		return seed, Exit(6, "prepare complete local Git seed snapshot: %v", err)
	}
	if err := checkLocalGitSeedPreflight(seed.Snapshot.Manifest, 0, cfg, force, stderr); err != nil {
		return seed, err
	}
	seed.Artifact, err = prepareLocalGitSeedArtifact(ctx, repo.Root, selection, 0)
	if err != nil {
		return seed, Exit(6, "prepare offline local Git metadata: %v", err)
	}
	if err := checkLocalGitSeedPreflight(seed.Snapshot.Manifest, seed.Artifact.ObjectBytes, cfg, force, stderr); err != nil {
		return seed, err
	}
	seed.Selection = selection
	seed.Plan = gitLocalSeedPlan{
		Head: selection.Head, Tree: selection.Tree,
		ObjectFormat: seed.Artifact.ObjectFormat, Digest: seed.Artifact.Digest,
		Refs: seed.Artifact.Refs, PackedBytes: seed.Artifact.PackedBytes,
	}
	if cfg.Sync.Fingerprint {
		seed.Plan.Fingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte("local-git-seed-v1\n"+seed.Snapshot.Fingerprint+"\n"+seed.Artifact.Digest)))
	}
	return seed, nil
}

func checkLocalGitSeedPreflight(manifest SyncManifest, objectBytes int64, cfg Config, force bool, stderr io.Writer) error {
	if objectBytes < 0 || manifest.Bytes < 0 || objectBytes > math.MaxInt64-manifest.Bytes {
		return Exit(6, "local Git seed size exceeds accounting limits")
	}
	guardrail := FullSyncGuardrailManifest(manifest)
	guardrail.Bytes += objectBytes
	return checkSyncPreflight(guardrail, cfg, force, stderr)
}

func transferLocalGitSeed(ctx context.Context, target SSHTarget, workdir string, seed preparedLocalGitSeed, timeout time.Duration, stderr io.Writer) error {
	if timeout <= 0 || timeout > localGitSeedTimeout {
		timeout = localGitSeedTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	bundle, err := os.Open(seed.Artifact.Path)
	if err != nil {
		return Exit(6, "open prepared local Git seed: %v", err)
	}
	defer bundle.Close()
	command := remoteGitLocalSeed(workdir, seed.Plan)
	if isWindowsNativeTarget(target) {
		command = windowsGitLocalSeed(workdir, seed.Plan)
	}
	if err := runSSHInput(ctx, target, command, bundle, io.Discard, stderr); err != nil {
		return Exit(6, "import local Git seed without origin fallback: %v", err)
	}
	return nil
}
