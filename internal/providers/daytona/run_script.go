package daytona

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	sdkdaytona "github.com/daytonaio/daytona/libs/sdk-go/pkg/daytona"
	core "github.com/openclaw/crabbox/internal/cli"
)

func uploadDaytonaRunScript(ctx context.Context, sandbox *sdkdaytona.Sandbox, commands *daytonaCommandRunner, workdir string, script *core.RunScriptSpec) (retErr error) {
	meta := path.Join(workdir, ".crabbox")
	dir := path.Join(meta, "scripts")
	staging := path.Join(dir, ".upload-"+newLeaseID())
	incoming := path.Join(staging, path.Base(script.RemotePath))
	destination := path.Join(workdir, script.RemotePath)
	guards := "test ! -L " + shellQuote(meta) + " && test ! -L " + shellQuote(dir)
	execute := func(ctx context.Context, action, command string) error {
		response, err := commands.ExecuteCommand(ctx, command)
		if err != nil {
			return fmt.Errorf("daytona %s script: %w", action, err)
		}
		if code := responseExitCode(response); code != 0 {
			return fmt.Errorf("daytona %s script exited %d", action, code)
		}
		return nil
	}
	// Upload into a private directory, then atomically publish the complete copy.
	// Repeated same-content runs must never truncate a script another run is reading.
	defer func() {
		if retErr == nil {
			return
		}
		cleanupCtx, cancel := daytonaCleanupContext()
		defer cancel()
		cleanup := guards + " && test ! -L " + shellQuote(staging) + " && if test -d " + shellQuote(staging) +
			"; then rm -f -- " + shellQuote(incoming) + " && rmdir -- " + shellQuote(staging) + "; fi"
		if err := execute(cleanupCtx, "clean up staged", cleanup); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("could not confirm script staging cleanup at %s: %w", staging, err))
		}
	}()
	prepare := "umask 077; " + guards + " && mkdir -p -- " + shellQuote(dir) +
		" && chmod 700 " + shellQuote(meta) + " " + shellQuote(dir) + " && mkdir -- " + shellQuote(staging)
	if err := execute(ctx, "prepare", prepare); err != nil {
		return err
	}
	if err := sandbox.FileSystem.UploadFileStream(ctx, bytes.NewReader(script.Data), incoming); err != nil {
		return fmt.Errorf("daytona upload script: %w", err)
	}
	return execute(ctx, "publish", "chmod 700 "+shellQuote(incoming)+
		" && test ! -d "+shellQuote(destination)+" && mv -f -- "+shellQuote(incoming)+" "+shellQuote(destination)+
		" && rmdir -- "+shellQuote(staging))
}

func daytonaCommandEnv(input map[string]string) (map[string]string, []string) {
	out := make(map[string]string, len(input))
	var stripped []string
	for name, value := range input {
		switch strings.ToUpper(strings.TrimSpace(name)) {
		case "DAYTONA_API_KEY", "CRABBOX_DAYTONA_API_KEY", "DAYTONA_CRABBOX_KEY", "DAYTONA_JWT_TOKEN", "CRABBOX_DAYTONA_JWT_TOKEN",
			"DAYTONA_ORGANIZATION_ID", "CRABBOX_DAYTONA_ORGANIZATION_ID", "CRABBOX_COORDINATOR_TOKEN":
			stripped = append(stripped, name)
		default:
			out[name] = value
		}
	}
	sort.Strings(stripped)
	return out, stripped
}
