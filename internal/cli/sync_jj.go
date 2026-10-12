package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Native jj owns revision selection. The temporary Git checkout only adapts
// that immutable tree to the existing manifest, seed and overlay machinery.
type jujutsuSource struct {
	sourceSnapshot
	WorkspaceRoot, Commit, Operation string
}

func jjOutput(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "jj", append([]string{"--repository", root, "--no-pager", "--color=never"}, args...)...)
	cmd.Env = childEnvironmentWithout(repositoryGitEnvironment(), "GIT_DIR", "GIT_WORK_TREE", "GIT_CEILING_DIRECTORIES")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		// Do not echo stderr: user-configured commands may print credentials.
		return "", fmt.Errorf("jj %s failed; install jj and use a current, non-stale Git-backed workspace with a single operation head: %w", args[0], err)
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

func jjGitCommand(ctx context.Context, root string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	cmd.Env = childEnvironmentWithout(repositoryGitEnvironment(), "GIT_DIR", "GIT_WORK_TREE", "GIT_CEILING_DIRECTORIES")
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	return cmd
}

func jjGitOutput(ctx context.Context, root string, args ...string) (string, error) {
	out, err := jjGitCommand(ctx, root, args...).Output()
	if err != nil {
		return "", fmt.Errorf("prepare native jj Git tree (%s): %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

func prepareJujutsuSource(ctx context.Context, repo Repo, cfg Config) (prepared Repo, source jujutsuSource, result error) {
	prepared = repo
	if explicitGitRepositoryRouting() {
		return
	}
	boundary, err := nearestRepositoryBoundary(repo.Root, "")
	if err != nil || boundary.kind != repositoryBoundaryNativeJujutsu {
		return prepared, source, err
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, source.cleanup())
			result = Exit(6, "native Jujutsu workspace %s: %v; resolve conflicts/divergence or use --no-sync", repo.Root, result)
		}
	}()
	if effectiveGitSeedSource(cfg) == "local" {
		return prepared, source, fmt.Errorf("native jj does not support --git-seed-source=local; use origin seeding or disable sync.gitSeed")
	}
	// Unlike ordinary jj commands, --at-op=@ rejects concurrent operation heads
	// instead of silently merging them. Check before snapshotting any edits.
	if _, err = jjOutput(ctx, repo.Root, "--at-op=@", "op", "log", "--no-graph", "--limit=1", "-T", "id"); err != nil {
		return prepared, source, err
	}
	state, err := jjOutput(ctx, repo.Root, "log", "--no-graph", "-r", "@", "-T", `commit_id ++ "\n" ++ conflict ++ "\n" ++ divergent`)
	if err != nil {
		return prepared, source, err
	}
	parts := strings.Split(state, "\n")
	if len(parts) != 3 || !validGitObjectID(parts[0]) || parts[1] != "false" || parts[2] != "false" {
		return prepared, source, fmt.Errorf("jj @ must resolve to one non-conflicted, non-divergent commit")
	}
	source.Commit = parts[0]
	source.Operation, err = jjOutput(ctx, repo.Root, "--at-op=@", "op", "log", "--no-graph", "--limit=1", "-T", "id")
	if err != nil || !validJJOperationID(source.Operation) {
		return prepared, source, fmt.Errorf("cannot pin jj operation: %w", err)
	}
	pinned := []string{"--at-op=" + source.Operation}
	state, err = jjOutput(ctx, repo.Root, append(pinned, "log", "--no-graph", "-r", "@", "-T", `commit_id ++ "\n" ++ conflict ++ "\n" ++ divergent`)...)
	if err != nil || state != strings.Join(parts, "\n") {
		return prepared, source, fmt.Errorf("jj working-copy revision changed during selection")
	}
	store, err := jjOutput(ctx, repo.Root, append(pinned, "git", "root")...)
	if err != nil || !filepath.IsAbs(store) {
		return prepared, source, fmt.Errorf("native jj requires a Git backend supported by `jj git root`: %w", err)
	}
	bookmarks, err := jjOutput(ctx, repo.Root, append(pinned, "bookmark", "list", "--remote=exact:origin", "-T", `if(remote, json({"name" => name, "remote" => remote, "conflict" => conflict, "commit" => normal_target.commit_id()}) ++ "\n")`)...)
	if err != nil {
		return prepared, source, err
	}
	refs, err := parseJJOriginBookmarks(bookmarks)
	if err != nil {
		return prepared, source, err
	}
	if subtree, err := managedStateSyncSubtree(repo.Root); err != nil || subtree != "" {
		return prepared, source, fmt.Errorf("native jj sync requires Crabbox state outside the workspace; configure an external state directory")
	}
	if pathWithinRoot(canonicalRepositoryPath(os.TempDir()), canonicalRepositoryPath(repo.Root)) {
		return prepared, source, fmt.Errorf("native jj sync requires TMPDIR outside the workspace")
	}
	snapshot, err := newSourceSnapshot()
	if err != nil {
		return prepared, source, err
	}
	source.sourceSnapshot = snapshot
	source.WorkspaceRoot = repo.Root
	root := snapshot.Root
	format, err := jjGitOutput(ctx, repo.Root, "--git-dir="+store, "rev-parse", "--show-object-format")
	if err != nil {
		return prepared, source, err
	}
	if _, err = jjGitOutput(ctx, root, "init", "--quiet", "--template=", "--object-format="+format); err != nil {
		return prepared, source, err
	}
	// No checkout filters, hooks or source Git configuration are imported.
	if strings.ContainsAny(store, "\r\n") {
		return prepared, source, fmt.Errorf("unsupported newline in jj Git store path")
	}
	if err = os.WriteFile(filepath.Join(root, ".git", "objects", "info", "alternates"), []byte(filepath.Join(store, "objects")+"\n"), 0600); err != nil {
		return prepared, source, err
	}
	for _, ref := range refs {
		if _, err = jjGitOutput(ctx, root, "check-ref-format", "refs/remotes/origin/"+ref.name); err != nil {
			return prepared, source, fmt.Errorf("unsupported jj origin bookmark name")
		}
		if _, err = jjGitOutput(ctx, root, "update-ref", "refs/remotes/origin/"+ref.name, ref.commit); err != nil {
			return prepared, source, err
		}
	}
	base, baseRef, err := jjSeedBase(ctx, root, source.Commit, cfg.Sync.BaseRef, refs)
	if err != nil {
		return prepared, source, err
	}
	if _, err = jjGitOutput(ctx, root, "update-ref", "HEAD", base); err != nil {
		return prepared, source, err
	}
	if _, err = jjGitOutput(ctx, root, "read-tree", source.Commit); err != nil {
		return prepared, source, err
	}
	if err = materializeJJTree(ctx, root, source.Commit); err != nil {
		return prepared, source, err
	}
	remote := ""
	if baseRef != "" {
		remote, err = jjGitOutput(ctx, repo.Root, "--git-dir="+store, "remote", "get-url", "origin")
		if err != nil {
			return prepared, source, fmt.Errorf("jj origin bookmarks require an origin remote: %w", err)
		}
		if gitRemoteURLHasCredentials(remote) {
			return prepared, source, fmt.Errorf("jj origin URL must not contain credentials; use a credential helper")
		}
		if _, err = jjGitOutput(ctx, root, "remote", "add", "origin", remote); err != nil {
			return prepared, source, err
		}
	}
	prepared = Repo{Root: root, Name: repoNameFromRootAndRemote(repo.Root, remote), Head: base, BaseRef: baseRef, RemoteURL: remote}
	return
}

type jjOriginBookmark struct{ name, commit string }

func parseJJOriginBookmarks(value string) ([]jjOriginBookmark, error) {
	var refs []jjOriginBookmark
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	seen := map[string]bool{}
	for {
		var ref struct {
			Name, Remote, Commit string
			Conflict             *bool
		}
		if err := decoder.Decode(&ref); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("unsupported jj origin bookmark output")
		}
		if ref.Name == "" || ref.Remote != "origin" || ref.Conflict == nil || *ref.Conflict || !validGitObjectID(ref.Commit) || seen[ref.Name] {
			return nil, fmt.Errorf("jj origin bookmarks must have single, non-conflicted targets")
		}
		seen[ref.Name] = true
		refs = append(refs, jjOriginBookmark{ref.Name, ref.Commit})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	return refs, nil
}

func validJJOperationID(value string) bool {
	if len(value) != 128 {
		return false
	}
	return strings.Trim(value, "0123456789abcdef") == ""
}

func jjSeedBase(ctx context.Context, root, commit, requested string, refs []jjOriginBookmark) (string, string, error) {
	for _, ref := range refs {
		if requested != "" && normalizedOriginBranch(requested) != ref.name {
			continue
		}
		base, err := jjGitOutput(ctx, root, "merge-base", "--all", commit, ref.commit)
		if err != nil {
			continue // Unrelated histories cannot supply a seed.
		}
		if !validGitObjectID(base) {
			return "", "", fmt.Errorf("ambiguous merge bases for jj origin bookmark %q", ref.name)
		}
		return base, "origin/" + ref.name, nil
	}
	if requested != "" {
		return "", "", fmt.Errorf("sync.baseRef %q must name a jj origin bookmark with shared ancestry", requested)
	}
	// With no advertised base, send the exact tree without remote seeding.
	return commit, "", nil
}

func materializeJJTree(ctx context.Context, root, commit string) error {
	fs, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer fs.Close()
	out, err := jjGitCommand(ctx, root, "ls-tree", "-r", "-z", "--full-tree", commit).Output()
	if err != nil {
		return fmt.Errorf("read jj commit tree: %w", err)
	}
	cmd := jjGitCommand(ctx, root, "cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	reader := bufio.NewReader(output)
	for _, entry := range splitNul(out) {
		meta, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || !safeRepoRel(path) || strings.EqualFold(strings.Split(path, "/")[0], ".git") || strings.EqualFold(strings.Split(path, "/")[0], ".jj") {
			return fmt.Errorf("unsafe jj tree path")
		}
		if fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755" && fields[0] != "120000") {
			return fmt.Errorf("unsupported jj tree entry %q (submodules require a materialized Git checkout)", path)
		}
		if _, err := fmt.Fprintln(in, fields[2]); err != nil {
			return err
		}
		header, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		info := strings.Fields(header)
		if len(info) != 3 || info[0] != fields[2] || info[1] != "blob" {
			return fmt.Errorf("invalid jj blob response")
		}
		size, err := strconv.ParseInt(info[2], 10, 64)
		if err != nil || size < 0 {
			return fmt.Errorf("invalid jj blob size")
		}
		rel := filepath.FromSlash(path)
		if err := fs.MkdirAll(filepath.Dir(rel), 0700); err != nil {
			return err
		}
		if fields[0] == "120000" {
			if size > 4096 {
				return fmt.Errorf("oversized jj symlink %q", path)
			}
			value := make([]byte, size)
			if _, err := io.ReadFull(reader, value); err != nil {
				return err
			}
			if err := fs.Symlink(string(value), rel); err != nil {
				return err
			}
		} else {
			mode := os.FileMode(0644)
			if fields[0] == "100755" {
				mode = 0755
			}
			file, err := fs.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, reader, size)
			if err := errors.Join(copyErr, file.Close()); err != nil {
				return err
			}
		}
		if b, err := reader.ReadByte(); err != nil || b != '\n' {
			return fmt.Errorf("invalid jj blob terminator")
		}
	}
	if err := in.Close(); err != nil {
		return err
	}
	return cmd.Wait()
}
