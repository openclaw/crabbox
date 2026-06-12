package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type cacheEntry struct {
	Kind  string `json:"kind"`
	Path  string `json:"path,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
	Note  string `json:"note,omitempty"`
}

func (a App) cacheVolumes(ctx context.Context, args []string) error {
	_ = ctx
	fs := newFlagSet("cache volumes", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(cfg.Cache.Volumes)
	}
	if len(cfg.Cache.Volumes) == 0 {
		fmt.Fprintln(a.Stdout, "no cache volumes configured")
		return nil
	}
	for _, volume := range cfg.Cache.Volumes {
		fmt.Fprintf(a.Stdout, "%-20s %-48s %s\n", blank(volume.Name, volume.Key), volume.Key, volume.Path)
	}
	return nil
}

func (a App) cacheStats(ctx context.Context, args []string) error {
	fs := newFlagSet("cache stats", a.Stderr)
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" && fs.NArg() > 0 {
		*id = fs.Arg(0)
	}
	if *id == "" {
		return exit(2, "usage: crabbox cache stats --id <lease-id-or-slug>")
	}
	target, cfg, _, err := a.cacheTarget(ctx, *id, *reclaim)
	if err != nil {
		return err
	}
	remote := remoteCacheStats(enabledCacheKinds(cfg.Cache))
	if isWindowsNativeTarget(target) {
		remote = windowsRemoteCacheUnsupported()
	}
	out, err := runSSHOutput(ctx, target, remote)
	if err != nil {
		return err
	}
	entries := parseCacheStats(out)
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(entries)
	}
	for _, entry := range entries {
		if entry.Note != "" {
			fmt.Fprintf(a.Stdout, "%-8s %s\n", entry.Kind, entry.Note)
			continue
		}
		fmt.Fprintf(a.Stdout, "%-8s %-32s %s\n", entry.Kind, formatBytes(entry.Bytes), entry.Path)
	}
	return nil
}

func (a App) cachePurge(ctx context.Context, args []string) error {
	fs := newFlagSet("cache purge", a.Stderr)
	id := fs.String("id", "", "lease id or slug")
	kind := fs.String("kind", "all", "cache kind: pnpm, npm, docker, git, or all")
	force := fs.Bool("force", false, "confirm purge")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" && fs.NArg() > 0 {
		*id = fs.Arg(0)
	}
	if *id == "" {
		return exit(2, "usage: crabbox cache purge --id <lease-id-or-slug> --kind <kind> --force")
	}
	if !*force {
		return exit(2, "cache purge requires --force")
	}
	target, cfg, leaseID, err := a.cacheTarget(ctx, *id, *reclaim)
	if err != nil {
		return err
	}
	enabled := enabledCacheKinds(cfg.Cache)
	if *kind != "all" && !enabled[*kind] {
		return exit(2, "cache kind %q is disabled by config", *kind)
	}
	if isWindowsNativeTarget(target) {
		return exit(2, "cache purge is not supported for target=windows windows.mode=normal")
	}
	if err := runSSHQuiet(ctx, target, remoteCachePurge(*kind, enabled)); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "purged cache kind=%s lease=%s\n", *kind, leaseID)
	return nil
}

func (a App) cacheWarm(ctx context.Context, args []string) error {
	fs := newFlagSet("cache warm", a.Stderr)
	id := fs.String("id", "", "lease id or slug")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	command := fs.Args()
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	if *id == "" {
		return exit(2, "cache warm requires --id")
	}
	if len(command) == 0 {
		return exit(2, "usage: crabbox cache warm --id <lease-id-or-slug> -- <command...>")
	}
	target, cfg, leaseID, err := a.cacheTarget(ctx, *id, *reclaim)
	if err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	workdir := remoteJoin(cfg, leaseID, repo.Name)
	actionsEnvFile := ""
	if state, err := readActionsHydrationState(ctx, target, leaseID); err == nil && state.Workspace != "" {
		workdir = state.Workspace
		actionsEnvFile = state.EnvFile
		fmt.Fprintf(a.Stderr, "using GitHub Actions workspace %s\n", workdir)
	}
	remote := remoteCacheWarmCommand(workdir, allowedEnv(cfg.EnvAllow), actionsEnvFile, command)
	if isWindowsNativeTarget(target) {
		remote = windowsRemoteCommandWithEnvFile(workdir, allowedEnv(cfg.EnvAllow), actionsEnvFile, command)
	}
	code := runSSHStream(ctx, target, remote, a.Stdout, a.Stderr)
	if code != 0 {
		return ExitError{Code: code, Message: fmt.Sprintf("cache warm command exited %d", code)}
	}
	return nil
}

func (a App) cacheTarget(ctx context.Context, id string, reclaim bool) (SSHTarget, Config, string, error) {
	cfg, err := loadConfig()
	if err != nil {
		return SSHTarget{}, Config{}, "", err
	}
	server, target, leaseID, err := a.resolveLeaseTargetWithConfig(ctx, &cfg, id)
	if err == nil {
		repo, repoErr := findRepo()
		if repoErr != nil {
			return SSHTarget{}, Config{}, "", repoErr
		}
		if claimErr := a.claimLeaseTargetForRepoAndRegister(ctx, leaseID, serverSlug(server), cfg, server, target, repo.Root, reclaim); claimErr != nil {
			return SSHTarget{}, Config{}, "", claimErr
		}
		a.touchLeaseTargetBestEffort(ctx, cfg, LeaseTarget{Server: server, SSH: target, LeaseID: leaseID}, "")
	}
	return target, cfg, leaseID, err
}

func enabledCacheKinds(cfg CacheConfig) map[string]bool {
	return map[string]bool{
		"pnpm":   cfg.Pnpm,
		"npm":    cfg.Npm,
		"docker": cfg.Docker,
		"git":    cfg.Git,
	}
}

func remoteCacheStats(enabled map[string]bool) string {
	items := []string{}
	if enabled["pnpm"] {
		items = append(items, "pnpm:/var/cache/crabbox/pnpm")
	}
	if enabled["npm"] {
		items = append(items, "npm:/var/cache/crabbox/npm")
	}
	if enabled["git"] {
		items = append(items, "git:/var/cache/crabbox/git")
	}
	var b strings.Builder
	if len(items) > 0 {
		b.WriteString("for item in")
		for _, item := range items {
			b.WriteByte(' ')
			b.WriteString(shellQuote(item))
		}
		b.WriteString("; do kind=${item%%:*}; path=${item#*:}; if [ -e \"$path\" ]; then bytes=$(du -sk \"$path\" 2>/dev/null | awk '{print $1*1024}'); printf '%s\\t%s\\t%s\\n' \"$kind\" \"$path\" \"${bytes:-0}\"; fi; done; ")
	}
	if enabled["docker"] {
		b.WriteString("if command -v docker >/dev/null 2>&1; then printf 'docker\\t\\t%s\\n' \"$(docker system df --format '{{.Type}}={{.Size}}' 2>/dev/null | paste -sd ',' -)\"; fi")
	}
	if b.Len() == 0 {
		return "true"
	}
	return b.String()
}

func remoteCacheWarmCommand(workdir string, env map[string]string, envFile string, command []string) string {
	return remoteCommandWithEnvFile(workdir, env, envFile, command)
}

func remoteCachePurge(kind string, enabled map[string]bool) string {
	if kind != "all" && !enabled[kind] {
		return "false"
	}
	commands := []string{}
	add := func(cacheKind, command string) {
		if enabled[cacheKind] {
			commands = append(commands, command)
		}
	}
	switch kind {
	case "pnpm":
		add("pnpm", "rm -rf /var/cache/crabbox/pnpm/*")
	case "npm":
		add("npm", "rm -rf /var/cache/crabbox/npm/*")
	case "git":
		add("git", "rm -rf /var/cache/crabbox/git/*")
	case "docker":
		add("docker", "docker system prune -af >/dev/null 2>&1 || true")
	case "all":
		add("pnpm", "rm -rf /var/cache/crabbox/pnpm/*")
		add("npm", "rm -rf /var/cache/crabbox/npm/*")
		add("git", "rm -rf /var/cache/crabbox/git/*")
		add("docker", "docker system prune -af >/dev/null 2>&1 || true")
	default:
		return "false"
	}
	if len(commands) == 0 {
		return "true"
	}
	return strings.Join(commands, "; ")
}

func parseCacheStats(output string) []cacheEntry {
	var entries []cacheEntry
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		entry := cacheEntry{Kind: parts[0], Path: parts[1]}
		if parts[1] == "" {
			entry.Note = parts[2]
		} else {
			fmt.Sscanf(parts[2], "%d", &entry.Bytes)
		}
		entries = append(entries, entry)
	}
	return entries
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
