package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/openclaw/crabbox/internal/runner/runnerfs"
	"github.com/openclaw/crabbox/internal/runner/runnerwire"
)

func TestClientWaitsForTransportExit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "result"), []byte("proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	late := errors.New("terminal exit not witnessed")
	client := Client{Identity: testIdentity(), Transport: func(ctx context.Context, input io.Reader, output io.Writer) error {
		if err := Serve(ctx, input, output, testIdentity()); err != nil {
			return err
		}
		return late
	}}
	results, err := client.Collect(t.Context(), root, []string{"result"}, false, "")
	if !errors.Is(err, late) || len(results.Files) != 0 {
		t.Fatalf("results=%v err=%v", results, err)
	}
}

func TestDownloadRejectsRemotePublicationTargetOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX archive endpoint")
	}
	source := filepath.Join(t.TempDir(), "report")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, archive, err := runnerfs.CreateArchive(t.Context(), source, runnerfs.CreateOptions{}, runnerfs.DefaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	data, err := io.ReadAll(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, forged := range []runnerfs.ArchiveSource{{Base: metadata.Base, ContentsOnly: true}, {Base: "unrelated"}} {
		t.Run(forged.Base+"-"+map[bool]string{true: "contents", false: "named"}[forged.ContentsOnly], func(t *testing.T) {
			destination := t.TempDir()
			sentinel := filepath.Join(destination, "unrelated")
			if err := os.WriteFile(sentinel, []byte("KEEP"), 0o600); err != nil {
				t.Fatal(err)
			}
			client := Client{Identity: testIdentity(), Transport: func(ctx context.Context, input io.Reader, output io.Writer) error {
				if _, err := io.Copy(io.Discard, input); err != nil {
					return err
				}
				if err := writeFrame(output, runnerwire.Hello, testIdentity(), 0, nil); err != nil {
					return err
				}
				if err := writeFrame(output, runnerwire.File, FileInfo{Archive: true, Source: forged}, uint64(len(data)), bytes.NewReader(data)); err != nil {
					return err
				}
				if err := writeFrame(output, runnerwire.Result, Outcome{}, 0, nil); err != nil {
					return err
				}
				return writeFrame(output, runnerwire.End, nil, 0, nil)
			}}
			if err := client.Download(t.Context(), source, destination); err == nil {
				t.Fatal("remote selected local publication target")
			}
			kept, err := os.ReadFile(sentinel)
			entries, listErr := os.ReadDir(destination)
			if err != nil || string(kept) != "KEEP" || listErr != nil || len(entries) != 1 {
				t.Fatalf("destination changed: kept=%q err=%v entries=%v listErr=%v", kept, err, entries, listErr)
			}
		})
	}
}

func TestClientTransportFailureUnblocksRequestWriter(t *testing.T) {
	failure := errors.New("cannot connect")
	client := Client{Identity: testIdentity(), Transport: func(context.Context, io.Reader, io.Writer) error { return failure }}
	if _, err := client.Collect(t.Context(), t.TempDir(), nil, true, ""); !errors.Is(err, failure) {
		t.Fatalf("err=%v", err)
	}
}

func TestClientReportsRemoteError(t *testing.T) {
	client := Client{Identity: testIdentity(), Transport: func(ctx context.Context, input io.Reader, output io.Writer) error {
		return Serve(ctx, input, output, testIdentity())
	}}
	_, err := client.Collect(t.Context(), filepath.Join(t.TempDir(), "missing"), nil, true, "")
	var remote RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("err=%v", err)
	}
}
