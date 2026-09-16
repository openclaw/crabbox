package shared

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
)

// CommandStreamRequest is the JSON command contract used by compatible runners.
// Authentication, endpoint selection, and HTTP deadlines belong to the adapter.
type CommandStreamRequest struct {
	Command   string            `json:"command"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	TimeoutMS int64             `json:"timeoutMs,omitempty"`
}

type commandStreamEvent struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	Error    string `json:"error,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

type CommandStream struct {
	Provider    string
	RedactError func(string) string
}

// Read consumes an admitted NDJSON response until the first completion or error.
// The caller owns HTTP status handling and closing the response. A completion
// already decoded wins over cancellation; EOF alone never proves command success.
func (stream CommandStream) Read(ctx context.Context, resp *http.Response, stdout, stderr io.Writer) (int, error) {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "" && mediaType != "application/x-ndjson" && mediaType != "application/jsonl" {
		return 0, fmt.Errorf("unexpected %s stream content-type %q", stream.Provider, resp.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	exitCode := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event commandStreamEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return exitCode, fmt.Errorf("decode %s stream event: %w", stream.Provider, err)
		}
		switch event.Type {
		case "stdout", "stderr":
			writer := stdout
			if event.Type == "stderr" {
				writer = stderr
			}
			if writer != nil {
				if _, err := io.WriteString(writer, event.Data); err != nil {
					return exitCode, fmt.Errorf("write %s %s: %w", stream.Provider, event.Type, err)
				}
			}
		case "complete":
			if event.ExitCode != nil {
				exitCode = *event.ExitCode
			}
			return exitCode, nil
		case "error":
			if event.Error == "" {
				event.Error = "stream error"
			}
			if stream.RedactError != nil {
				event.Error = stream.RedactError(event.Error)
			}
			return exitCode, errors.New(event.Error)
		case "start", "heartbeat":
		default:
			return exitCode, fmt.Errorf("unknown %s stream event %q", stream.Provider, event.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return exitCode, err
	}
	if err := ctx.Err(); err != nil {
		return exitCode, err
	}
	return exitCode, fmt.Errorf("%s stream ended before completion", stream.Provider)
}
