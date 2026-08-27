package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/openclaw/crabbox/internal/runner/runnerfs"
	"github.com/openclaw/crabbox/internal/runner/runnerwire"
)

// Serve handles exactly one operation. Loss of the response after publication
// remains ambiguous to the caller; it must not infer success or blindly retry.
func Serve(ctx context.Context, input io.Reader, output io.Writer, identity Identity) error {
	if err := writeFrame(output, runnerwire.Hello, identity, 0, nil); err != nil {
		return err
	}
	outcome, err := serveOperation(ctx, input, output, identity)
	if err != nil {
		code := "operation"
		var invalid runnerfs.InvalidArchiveError
		if errors.As(err, &invalid) {
			code = "invalid"
		}
		writeErr := writeFrame(output, runnerwire.Error, RemoteError{Code: code, Message: err.Error()}, 0, nil)
		if writeErr == nil {
			writeErr = writeFrame(output, runnerwire.End, nil, 0, nil)
		}
		return errors.Join(err, writeErr)
	}
	if err := writeFrame(output, runnerwire.Result, outcome, 0, nil); err != nil {
		return err
	}
	return writeFrame(output, runnerwire.End, nil, 0, nil)
}

func serveOperation(ctx context.Context, input io.Reader, output io.Writer, identity Identity) (Outcome, error) {
	limits := runnerfs.DefaultArchiveLimits()
	frames := runnerwire.NewReader(input, uint64(limits.MaxCompressedBytes))
	frame, err := frames.Next()
	if err != nil {
		return Outcome{}, err
	}
	if frame.Header.Kind != runnerwire.Request {
		return Outcome{}, errors.New("missing runner request")
	}
	var request Request
	if err := decodeObject(frame.Header.Meta, &request); err != nil {
		return Outcome{}, err
	}
	if request.BuildID == "" || request.BuildID != identity.BuildID {
		return Outcome{}, errors.New("runner build identity mismatch")
	}
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	if request.Operation == Upload {
		if runtime.GOOS == "windows" {
			return Outcome{}, errors.New("archive publication requires a POSIX target")
		}
		if frame.Header.Size == 0 {
			return Outcome{}, errors.New("upload request has no archive")
		}
		destination, err := expandPath(request.Destination)
		if err != nil {
			return Outcome{}, err
		}
		target, err := runnerfs.ArchiveTarget(destination, request.Source.Base, request.Source.ContentsOnly)
		if err != nil {
			return Outcome{}, err
		}
		stage, err := runnerfs.StageArchive(ctx, frame.Body, target, runnerfs.ArchivePayloadRoot, runnerfs.ExtractOptions{PreservePermissions: true}, limits)
		if err != nil {
			return Outcome{}, err
		}
		defer stage.Close()
		if err := readEnd(frames); err != nil {
			return Outcome{}, err
		}
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}
		if err := stage.Publish(); err != nil {
			return Outcome{}, err
		}
		return Outcome{Published: true}, nil
	}
	if frame.Header.Size != 0 {
		return Outcome{}, errors.New("operation does not accept a request payload")
	}
	if err := readEnd(frames); err != nil {
		return Outcome{}, err
	}
	switch request.Operation {
	case Collect:
		return serveResults(ctx, request, output)
	case Download:
		if runtime.GOOS == "windows" {
			return Outcome{}, errors.New("archive download requires a POSIX target")
		}
		if _, err := runnerfs.DownloadSource(request.SourcePath); err != nil {
			return Outcome{}, err
		}
		sourcePath, err := expandPath(request.SourcePath)
		if err != nil {
			return Outcome{}, err
		}
		source, archive, err := runnerfs.CreateArchive(ctx, sourcePath, runnerfs.CreateOptions{RejectHardLinks: true}, limits)
		if err != nil {
			return Outcome{}, err
		}
		defer os.Remove(archive.Name())
		defer archive.Close()
		info, err := archive.Stat()
		if err != nil {
			return Outcome{}, err
		}
		if err := writeFrame(output, runnerwire.File, FileInfo{Archive: true, Source: source}, uint64(info.Size()), archive); err != nil {
			return Outcome{}, err
		}
		return Outcome{}, nil
	default:
		return Outcome{}, errors.New("unknown runner operation")
	}
}

func serveResults(ctx context.Context, request Request, output io.Writer) (Outcome, error) {
	if err := validateResultPaths(request.Paths); err != nil {
		return Outcome{}, err
	}
	workdir, err := expandPath(request.Workdir)
	if err != nil {
		return Outcome{}, err
	}
	root, err := runnerfs.OpenRoot(workdir)
	if err != nil {
		return Outcome{}, err
	}
	defer root.Close()
	options := runnerfs.ResultOptions{Paths: request.Paths, Auto: request.Auto, ExplicitMaxBytes: ExplicitMaxFileBytes, ExplicitTotalBytes: ExplicitMaxTotalBytes}
	if options.Auto && request.Marker != "" {
		if request.Marker != "crabbox/results-start" {
			return Outcome{}, errors.New("unsupported results marker")
		}
		options.After, err = resultsMarkerTime(ctx, workdir)
		if err != nil {
			options.Auto = false
		}
	}
	results, err := root.CollectResults(ctx, options)
	if err != nil {
		return Outcome{}, err
	}
	for _, file := range results.Files {
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}
		if err := writeFrame(output, runnerwire.File, FileInfo{Path: file.Path, ModTime: file.ModTime}, uint64(len(file.Data)), bytes.NewReader(file.Data)); err != nil {
			return Outcome{}, err
		}
	}
	return Outcome{Warnings: boundedResultWarnings(results.Warnings)}, nil
}

func boundedResultWarnings(warnings []runnerfs.Warning) []runnerfs.Warning {
	const budget = 48 << 10 // Leave room for the frame and an overflow summary.
	retained := make([]runnerfs.Warning, 0, min(len(warnings), 128))
	used, omitted := 0, 0
	for _, warning := range warnings {
		encoded, _ := json.Marshal(warning)
		if len(encoded)+1 > budget-used {
			omitted++
			continue
		}
		used += len(encoded) + 1
		retained = append(retained, warning)
	}
	if omitted != 0 {
		retained = append(retained, runnerfs.Warning{Message: fmt.Sprintf("%d additional result warnings omitted to keep the response bounded", omitted)})
	}
	return retained
}

// Retain the existing Git-worktree marker location until execution owns marker
// creation. This reads only its timestamp, never external file contents.
func resultsMarkerTime(ctx context.Context, workdir string) (time.Time, error) {
	resolved, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		return time.Time{}, err
	}
	workdir, err = filepath.Abs(resolved)
	if err != nil {
		return time.Time{}, err
	}
	marker := filepath.Join(workdir, ".crabbox", "results-start")
	command := exec.CommandContext(ctx, "git", "rev-parse", "--git-path", "crabbox/results-start")
	command.Dir = workdir
	var output bytes.Buffer
	command.Stdout = &limitedOutput{writer: &output, remaining: 16 << 10}
	if err := command.Run(); err == nil {
		name := strings.TrimSpace(output.String())
		if name != "" {
			marker = name
			if !filepath.IsAbs(marker) {
				marker = filepath.Join(workdir, marker)
			}
		}
	}
	info, err := os.Stat(marker)
	if err != nil {
		return time.Time{}, err
	}
	if !info.Mode().IsRegular() {
		return time.Time{}, errors.New("results marker is not regular")
	}
	return info.ModTime(), nil
}

type limitedOutput struct {
	writer    io.Writer
	remaining int
}

func (w *limitedOutput) Write(data []byte) (int, error) {
	if len(data) > w.remaining {
		return 0, errors.New("runner subprocess output exceeds limit")
	}
	n, err := w.writer.Write(data)
	w.remaining -= n
	return n, err
}

func expandPath(name string) (string, error) {
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\x00\r\n") {
		return "", errors.New("runner path is empty or contains control characters")
	}
	if strings.HasPrefix(name, "~") {
		if name != "~" && !strings.HasPrefix(name, "~/") {
			return "", errors.New("named-user home paths are unsupported")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return home + strings.TrimPrefix(name, "~"), nil
	}
	return name, nil
}
