package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"reflect"

	"github.com/openclaw/crabbox/internal/runner/runnerfs"
)

// Transport runs one installed helper. It must honor cancellation, wait for the
// exact command's terminal result, and return an error on nonzero or unknown
// exit. Authentication, installation, and provider authority stay in adapters.
type Transport func(context.Context, io.Reader, io.Writer) error

type Client struct {
	Identity  Identity
	Transport Transport
}

type Factory func(context.Context) (*Client, error)

func (client *Client) Collect(ctx context.Context, workdir string, paths []string, auto bool, marker string) (runnerfs.Results, error) {
	if err := validateResultPaths(paths); err != nil {
		return runnerfs.Results{}, err
	}
	var results runnerfs.Results
	outcome, err := client.invoke(ctx, Request{Operation: Collect, Workdir: workdir, Paths: paths, Auto: auto, Marker: marker}, 0, nil, func(info FileInfo, input io.Reader) error {
		data, err := io.ReadAll(input)
		if err != nil {
			return err
		}
		results.Files = append(results.Files, runnerfs.File{Path: info.Path, ModTime: info.ModTime, Data: data})
		return nil
	})
	if err != nil {
		return runnerfs.Results{}, err
	}
	results.Warnings = outcome.Warnings
	return results, nil
}

func (client *Client) Upload(ctx context.Context, source, destination string, followLinks bool) error {
	metadata, archive, err := runnerfs.CreateArchive(ctx, source, runnerfs.CreateOptions{FollowLinks: followLinks}, runnerfs.DefaultArchiveLimits())
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		return err
	}
	_, err = client.invoke(ctx, Request{Operation: Upload, Destination: destination, Source: metadata}, uint64(info.Size()), archive, nil)
	return err
}

func (client *Client) Download(ctx context.Context, source, destination string) error {
	requested, err := runnerfs.DownloadSource(source)
	if err != nil {
		return err
	}
	var stage *runnerfs.StagedArchive
	defer func() {
		if stage != nil {
			_ = stage.Close()
		}
	}()
	_, err = client.invoke(ctx, Request{Operation: Download, SourcePath: source}, 0, nil, func(info FileInfo, input io.Reader) error {
		if info.Source != requested {
			return errors.New("remote archive metadata differs from requested source")
		}
		target, err := runnerfs.ArchiveTarget(destination, requested.Base, requested.ContentsOnly)
		if err != nil {
			return err
		}
		stage, err = runnerfs.StageArchive(ctx, input, target, runnerfs.ArchivePayloadRoot, runnerfs.ExtractOptions{}, runnerfs.DefaultArchiveLimits())
		return err
	})
	if err != nil {
		return err
	}
	if stage == nil {
		return errors.New("runner download did not stage an archive")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return stage.Publish()
}

func (client *Client) invoke(ctx context.Context, request Request, size uint64, body io.Reader, consume func(FileInfo, io.Reader) error) (Outcome, error) {
	if client == nil || client.Transport == nil {
		return Outcome{}, errors.New("runner transport is unavailable")
	}
	request.BuildID = client.Identity.BuildID
	if err := validateMetadataText(reflect.ValueOf(request)); err != nil {
		return Outcome{}, err
	}
	var outcome Outcome
	err := exchange(ctx, client.Transport, func(output io.Writer) error {
		return WriteRequest(output, request, size, body)
	}, func(ctx context.Context, input io.Reader) error {
		var err error
		outcome, err = ReadResponse(ctx, input, client.Identity, request.Operation, consume)
		return err
	})
	if err != nil {
		return Outcome{}, err
	}
	return outcome, nil
}

// Base64Transport adapts UTF-8-only exec/log channels to the binary protocol.
// The supplied transport must invoke the helper's serve-base64 entrypoint.
func Base64Transport(transport Transport) Transport {
	return func(ctx context.Context, input io.Reader, output io.Writer) error {
		return exchange(ctx, transport, func(encoded io.Writer) error {
			encoder := base64.NewEncoder(base64.StdEncoding, encoded)
			_, err := io.Copy(encoder, input)
			return errors.Join(err, encoder.Close())
		}, func(_ context.Context, encoded io.Reader) error {
			_, err := io.Copy(output, base64.NewDecoder(base64.StdEncoding, encoded))
			return err
		})
	}
}

func exchange(ctx context.Context, transport Transport, write func(io.Writer) error, read func(context.Context, io.Reader) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	writeDone := make(chan error, 1)
	go func() {
		err := write(requestWriter)
		_ = requestWriter.CloseWithError(err)
		if err != nil {
			cancel(err)
		}
		writeDone <- err
	}()
	readDone := make(chan error, 1)
	go func() {
		err := read(ctx, responseReader)
		_ = responseReader.CloseWithError(err)
		if err != nil {
			cancel(err)
		}
		readDone <- err
	}()
	transportErr := transport(ctx, requestReader, responseWriter)
	_ = responseWriter.CloseWithError(transportErr)
	_ = requestReader.CloseWithError(io.ErrClosedPipe)
	if transportErr != nil {
		cancel(transportErr)
	}
	written := <-writeDone
	readErr := <-readDone
	if err := errors.Join(readErr, transportErr, written); err != nil {
		return err
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return nil
}
