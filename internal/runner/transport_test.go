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
	"time"

	"github.com/openclaw/crabbox/internal/runner/runnerfs"
	"github.com/openclaw/crabbox/internal/runner/runnerwire"
)

func TestExchangeCancellationReleasesInputAndResponseEOF(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cause := errors.New("caller stopped transfer")
	inputReady := make(chan struct{})
	responseReady := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- exchange(ctx, func(_ context.Context, input io.Reader, output io.Writer) error {
			close(inputReady)
			_, err := io.Copy(io.Discard, input)
			return err
		}, func(output io.Writer) error {
			// The producer has nothing more to write until its context is canceled.
			<-ctx.Done()
			return context.Cause(ctx)
		}, func(_ context.Context, input io.Reader) error {
			close(responseReady)
			_, err := io.Copy(io.Discard, input)
			return err
		})
	}()
	<-inputReady
	<-responseReady
	cancel(cause)
	select {
	case err := <-done:
		if !errors.Is(err, cause) {
			t.Fatalf("lost caller cause: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not release stream I/O")
	}
}

func TestExchangeCancellationPreservesCauseAlongsideTransportFailure(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cause := errors.New("caller canceled request")
	transportErr := errors.New("transport stopped")
	err := exchange(ctx, func(_ context.Context, input io.Reader, output io.Writer) error {
		cancel(cause)
		return transportErr
	}, func(output io.Writer) error {
		_, err := output.Write([]byte("request"))
		return err
	}, func(_ context.Context, input io.Reader) error {
		_, err := io.Copy(io.Discard, input)
		return err
	})
	if !errors.Is(err, cause) || !errors.Is(err, transportErr) {
		t.Fatalf("want caller cause and transport failure, got %v", err)
	}
}

func TestExchangeOrdinaryEOFCompletes(t *testing.T) {
	err := exchange(context.Background(), func(_ context.Context, input io.Reader, output io.Writer) error {
		_, err := io.Copy(output, input)
		return err
	}, func(output io.Writer) error {
		_, err := io.WriteString(output, "ordinary transfer")
		return err
	}, func(_ context.Context, input io.Reader) error {
		data, err := io.ReadAll(input)
		if err == nil && string(data) != "ordinary transfer" {
			return errors.New("incomplete response")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExchangePreservesCallerCauseAfterProducerFailure(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	callerErr := errors.New("caller stopped request")
	producerErr := errors.New("input unavailable")
	err := exchange(ctx, func(transportCtx context.Context, input io.Reader, output io.Writer) error {
		<-transportCtx.Done()
		cancel(callerErr)
		return transportCtx.Err()
	}, func(io.Writer) error { return producerErr }, func(_ context.Context, input io.Reader) error {
		_, err := io.Copy(io.Discard, input)
		return err
	})
	if !errors.Is(err, callerErr) || !errors.Is(err, producerErr) {
		t.Fatalf("want caller and producer causes, got %v", err)
	}
}

func copyTransportFixture(t *testing.T) (string, *Client) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("archive publication requires POSIX")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	if err := os.MkdirAll(filepath.Join(root, "Library", "Caches"), 0700); err != nil {
		t.Fatal(err)
	}
	identity := CurrentIdentity()
	return root, &Client{Identity: identity, Transport: func(ctx context.Context, input io.Reader, output io.Writer) error {
		return Serve(ctx, input, output, identity)
	}}
}

func assertCopyDestinationUnchanged(t *testing.T, target string) {
	t.Helper()
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "previous contents" {
		t.Fatalf("destination = %q, %v; want previous contents", data, err)
	}
	stages, err := filepath.Glob(filepath.Join(filepath.Dir(target), ".crabbox-cp-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("private staging directories remain: %v, %v", stages, err)
	}
}

func TestUploadInterruptedRequestPreservesDestination(t *testing.T) {
	for _, point := range []string{"during archive", "before terminal end"} {
		t.Run(point, func(t *testing.T) {
			root, client := copyTransportFixture(t)
			source, target := filepath.Join(root, "source"), filepath.Join(root, "destination")
			if err := os.WriteFile(source, bytes.Repeat([]byte("ordinary incoming contents\n"), 1000), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte("previous contents"), 0600); err != nil {
				t.Fatal(err)
			}
			metadata, archive, err := runnerfs.CreateArchive(t.Context(), source, runnerfs.CreateOptions{}, runnerfs.DefaultArchiveLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(archive.Name())
			defer archive.Close()
			info, err := archive.Stat()
			if err != nil {
				t.Fatal(err)
			}
			var request bytes.Buffer
			if err := writeFrame(&request, runnerwire.Request, Request{BuildID: client.Identity.BuildID, Operation: Upload, Destination: target, Source: metadata}, uint64(info.Size()), archive); err != nil {
				t.Fatal(err)
			}
			// Cut an ordinary request either inside its archive or after its entire
			// payload, as a connection closing before the terminal frame would do.
			length := request.Len()
			if point == "during archive" {
				length -= int(info.Size() / 2)
			}
			if err := Serve(t.Context(), bytes.NewReader(request.Bytes()[:length]), io.Discard, client.Identity); err == nil {
				t.Fatal("interrupted upload accepted")
			}
			assertCopyDestinationUnchanged(t, target)
		})
	}
}

func TestCopyDirectoryIntentThroughClientAndServer(t *testing.T) {
	for _, operation := range []string{"upload", "download"} {
		for _, intent := range []string{"existing directory", "new directory with trailing slash", "source contents"} {
			t.Run(operation+"/"+intent, func(t *testing.T) {
				root, client := copyTransportFixture(t)
				source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
				if err := os.Mkdir(source, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(source, "report.txt"), []byte("ordinary report"), 0600); err != nil {
					t.Fatal(err)
				}
				want := filepath.Join(destination, "source", "report.txt")
				switch intent {
				case "existing directory":
					if err := os.Mkdir(destination, 0700); err != nil {
						t.Fatal(err)
					}
				case "new directory with trailing slash":
					destination += "/"
				case "source contents":
					source += "/"
					want = filepath.Join(destination, "report.txt")
				}
				var err error
				if operation == "upload" {
					err = client.Upload(t.Context(), source, destination, false)
				} else {
					err = client.Download(t.Context(), source, destination)
				}
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(want)
				if err != nil || string(data) != "ordinary report" {
					t.Fatalf("copied file = %q, %v", data, err)
				}
				entries, err := os.ReadDir(destination)
				if err != nil || len(entries) != 1 {
					t.Fatalf("destination entries = %v, %v; want one copied entry", entries, err)
				}
			})
		}
	}
}

func TestDownloadTransportFailureAfterResponsePreservesDestination(t *testing.T) {
	root, client := copyTransportFixture(t)
	source, target := filepath.Join(root, "source"), filepath.Join(root, "destination")
	if err := os.WriteFile(source, []byte("incoming contents"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("previous contents"), 0600); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("transport exit failed after response")
	served := false
	client.Transport = func(ctx context.Context, input io.Reader, output io.Writer) error {
		if err := Serve(ctx, input, output, client.Identity); err != nil {
			return err
		}
		served = true
		return failed
	}
	if err := client.Download(t.Context(), source, target); !errors.Is(err, failed) {
		t.Fatalf("download error = %v; want transport failure", err)
	}
	if !served {
		t.Fatal("server did not finish writing its complete response")
	}
	assertCopyDestinationUnchanged(t, target)
}
