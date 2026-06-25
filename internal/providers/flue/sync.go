package flue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type flueRunPayload struct {
	RequestFile string
	ArchiveFile *os.File
	Request     Request
	SyncPhases  []timingPhase
	SyncTotal   time.Duration
	Cleanup     func()
}

func buildFlueRunPayload(ctx context.Context, cfg Config, req RunRequest, leaseID, slug string, started time.Time, stderr io.Writer, now func() time.Time) (flueRunPayload, error) {
	if strings.TrimSpace(req.Repo.Root) == "" {
		return flueRunPayload{}, exit(2, "provider=%s requires a local git workspace for archive sync", providerName)
	}
	workdir, err := cleanWorkdir(cfg.Flue.Workdir)
	if err != nil {
		return flueRunPayload{}, err
	}
	command, err := flueCommand(req.Command, req.ShellMode)
	if err != nil {
		return flueRunPayload{}, err
	}
	syncStart := now()
	excludes, err := syncExcludes(req.Repo.Root, cfg)
	if err != nil {
		return flueRunPayload{}, err
	}
	manifestStart := now()
	manifest, err := syncManifest(req.Repo.Root, excludes, cfg.Sync.Includes)
	if err != nil {
		return flueRunPayload{}, exit(6, "build sync file list: %v", err)
	}
	manifestDuration := now().Sub(manifestStart)
	preflightStart := now()
	archiveManifest := manifest
	archiveManifest.Changed = nil
	archiveManifest.ChangedBytes = 0
	if stderr == nil {
		stderr = io.Discard
	}
	if err := checkSyncPreflight(archiveManifest, cfg, req.ForceSyncLarge, stderr); err != nil {
		return flueRunPayload{}, err
	}
	preflightDuration := now().Sub(preflightStart)
	archiveStart := now()
	archive, err := createPortableSyncArchive(ctx, req.Repo, manifest, "crabbox-flue-sync-*.tgz")
	if err != nil {
		return flueRunPayload{}, err
	}
	cleanupArchive := true
	cleanup := func() {
		if archive != nil {
			name := archive.Name()
			_ = archive.Close()
			if cleanupArchive {
				_ = os.Remove(name)
			}
		}
	}
	archiveDuration := now().Sub(archiveStart)
	request := Request{
		ProtocolVersion:  protocolVersion,
		Operation:        operationRun,
		LeaseID:          leaseID,
		Slug:             slug,
		Workflow:         strings.TrimSpace(cfg.Flue.Workflow),
		Target:           strings.ToLower(blank(strings.TrimSpace(cfg.Flue.Target), defaultTarget)),
		WorkspaceArchive: archive.Name(),
		Workspace:        workdir,
		Command:          command,
		Env:              req.Env,
		TimeoutMs:        int64(cfg.Flue.TimeoutSecs) * int64(time.Second/time.Millisecond),
		Metadata: map[string]string{
			"provider": providerName,
			"repo":     strings.TrimSpace(req.Repo.Name),
			"started":  started.UTC().Format(time.RFC3339Nano),
		},
		OutputLimits: OutputLimits{
			StdoutBytes: defaultStdoutLimitBytes,
			StderrBytes: defaultStderrLimitBytes,
		},
	}
	if request.TimeoutMs == 0 {
		request.TimeoutMs = 0
	}
	if err := request.Validate(); err != nil {
		cleanup()
		return flueRunPayload{}, err
	}
	requestFile, err := writeFlueRequestFile(request)
	if err != nil {
		cleanup()
		return flueRunPayload{}, err
	}
	cleanupRequest := true
	cleanupAll := func() {
		if cleanupRequest {
			_ = os.Remove(requestFile)
		}
		cleanup()
	}
	syncTotal := now().Sub(syncStart)
	return flueRunPayload{
		RequestFile: requestFile,
		ArchiveFile: archive,
		Request:     request,
		SyncPhases: []timingPhase{
			{Name: "manifest", Ms: manifestDuration.Milliseconds()},
			{Name: "preflight", Ms: preflightDuration.Milliseconds()},
			{Name: "archive", Ms: archiveDuration.Milliseconds()},
			{Name: "flue_archive_sync", Ms: syncTotal.Milliseconds()},
		},
		SyncTotal: syncTotal,
		Cleanup:   cleanupAll,
	}, nil
}

func writeFlueRequestFile(request Request) (string, error) {
	file, err := os.CreateTemp("", "crabbox-flue-request-*.json")
	if err != nil {
		return "", fmt.Errorf("create flue request temp file: %w", err)
	}
	path := file.Name()
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", fmt.Errorf("secure flue request temp file: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(request); err != nil {
		return "", fmt.Errorf("write flue request temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close flue request temp file: %w", err)
	}
	keep = true
	return path, nil
}

func flueCommand(command []string, shellMode bool) ([]string, error) {
	if len(command) == 0 {
		return nil, exit(2, "missing command")
	}
	if shellMode {
		return []string{"/bin/sh", "-lc", strings.Join(command, " ")}, nil
	}
	if len(command) == 1 && shouldUseShell(command) {
		return []string{"/bin/sh", "-lc", command[0]}, nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		return []string{"/bin/sh", "-lc", shellScriptFromArgv(command)}, nil
	}
	return append([]string(nil), command...), nil
}

func flueCommandText(command []string, shellMode bool) (string, error) {
	if len(command) == 0 {
		return "", exit(2, "missing command")
	}
	if shellMode {
		return strings.Join(command, " "), nil
	}
	if len(command) == 1 && shouldUseShell(command) {
		return command[0], nil
	}
	return shellScriptFromArgv(command), nil
}

func flueEnvRedactions(env map[string]string) []string {
	values := make([]string, 0, len(env))
	for _, value := range env {
		if strings.TrimSpace(value) != "" {
			values = append(values, value)
		}
	}
	return values
}
