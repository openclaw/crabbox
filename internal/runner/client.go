package runner

import (
	"context"
	"errors"
	"io"

	"github.com/openclaw/crabbox/internal/runner/runnerfs"
	"github.com/openclaw/crabbox/internal/runner/runnerwire"
)

func WriteRequest(output io.Writer, request Request, size uint64, body io.Reader) error {
	if request.BuildID == "" {
		return errors.New("runner request requires a build identity")
	}
	if request.Operation == Collect {
		if err := validateResultPaths(request.Paths); err != nil {
			return err
		}
	}
	if request.Operation != Upload && size != 0 {
		return errors.New("runner operation cannot send a payload")
	}
	if err := writeFrame(output, runnerwire.Request, request, size, body); err != nil {
		return err
	}
	return writeFrame(output, runnerwire.End, nil, 0, nil)
}

// ReadResponse validates the stream before reporting completion. A file consumer
// must only stage bytes: publication requires both a returned outcome and a
// successful transport exit. The stream alone cannot prove transport success.
func ReadResponse(ctx context.Context, input io.Reader, expected Identity, operation Operation, consume func(FileInfo, io.Reader) error) (Outcome, error) {
	maxBody := uint64(ExplicitMaxFileBytes)
	if operation == Download {
		maxBody = uint64(runnerfs.DefaultArchiveLimits().MaxCompressedBytes)
	}
	frames := runnerwire.NewReader(input, maxBody)
	frame, err := frames.Next()
	if err != nil {
		return Outcome{}, err
	}
	if frame.Header.Kind != runnerwire.Hello {
		return Outcome{}, errors.New("runner response has no identity")
	}
	var actual Identity
	if err := decodeObject(frame.Header.Meta, &actual); err != nil {
		return Outcome{}, err
	}
	if expected.BuildID == "" || actual != expected || actual.Protocol != runnerwire.Version {
		return Outcome{}, errors.New("runner response identity mismatch")
	}
	count := 0
	var total uint64
	seen := make(map[string]bool)
	for {
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}
		frame, err = frames.Next()
		if err != nil {
			return Outcome{}, err
		}
		switch frame.Header.Kind {
		case runnerwire.File:
			count++
			var info FileInfo
			if err := decodeObject(frame.Header.Meta, &info); err != nil {
				return Outcome{}, err
			}
			if operation == Upload {
				return Outcome{}, errors.New("upload returned an unexpected file")
			}
			if operation == Download {
				if count != 1 || !info.Archive {
					return Outcome{}, errors.New("download returned an invalid archive frame")
				}
			} else {
				if operation != Collect || info.Archive || info.Path == "" || seen[info.Path] || count > ExplicitMaxFiles+runnerfs.AutoMaxFiles {
					return Outcome{}, errors.New("invalid runner result file")
				}
				seen[info.Path] = true
				if frame.Header.Size > ExplicitMaxTotalBytes+runnerfs.AutoMaxTotalBytes-total {
					return Outcome{}, errors.New("runner results exceed aggregate limit")
				}
				total += frame.Header.Size
			}
			if consume == nil {
				return Outcome{}, errors.New("runner file has no consumer")
			}
			if err := consume(info, frame.Body); err != nil {
				return Outcome{}, err
			}
		case runnerwire.Result:
			var outcome Outcome
			if err := decodeObject(frame.Header.Meta, &outcome); err != nil {
				return Outcome{}, err
			}
			if operation == Download && count != 1 {
				return Outcome{}, errors.New("runner download omitted its archive")
			}
			if outcome.Published != (operation == Upload) {
				return Outcome{}, errors.New("invalid runner publication outcome")
			}
			if err := readEnd(frames); err != nil {
				return Outcome{}, err
			}
			return outcome, nil
		case runnerwire.Error:
			var remote RemoteError
			if err := decodeObject(frame.Header.Meta, &remote); err != nil {
				return Outcome{}, err
			}
			if remote.Code == "" || remote.Message == "" {
				return Outcome{}, errors.New("invalid runner error")
			}
			if err := readEnd(frames); err != nil {
				return Outcome{}, errors.Join(remote, err)
			}
			return Outcome{}, remote
		default:
			return Outcome{}, errors.New("unexpected runner response frame")
		}
	}
}
