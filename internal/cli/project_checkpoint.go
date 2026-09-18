package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
)

// Only metadata crosses the CLI. Project bytes travel over the coordinator's
// authenticated private provider transport and remain encrypted in its store.
type ProjectCheckpointRequest struct {
	Action             string `json:"action"`
	Target             string `json:"target,omitempty"`
	Authority          string `json:"authority,omitempty"`
	ProjectID          string `json:"projectID,omitempty"`
	SessionID          string `json:"sessionID,omitempty"`
	Root               string `json:"root,omitempty"`
	AcceptedRevision   string `json:"acceptedRevision,omitempty"`
	DependencyPolicy   string `json:"dependencyPolicy,omitempty"`
	PreviousGeneration int64  `json:"previousGeneration,omitempty"`
	Generation         int64  `json:"generation,omitempty"`
	Kind               string `json:"kind,omitempty"`
	ConfirmDiscard     bool   `json:"confirmDiscard,omitempty"`
}

func (c *CoordinatorClient) ProjectCheckpoint(ctx context.Context, id string, input *ProjectCheckpointRequest) (json.RawMessage, error) {
	method := http.MethodGet
	var body any
	if input != nil {
		method, body = http.MethodPost, input
	}
	var response json.RawMessage
	err := c.do(ctx, method, "/v1/leases/"+url.PathEscape(id)+"/project-checkpoint", body, &response)
	return response, err
}

func (a App) projectCheckpoint(ctx context.Context, args []string) error {
	fs := newFlagSet("project-checkpoint", a.Stderr)
	id := fs.String("id", "", "coordinator lease id")
	requestStdin := fs.Bool("request-stdin", false, "read bounded checkpoint action metadata from stdin")
	jsonOut := fs.Bool("json", false, "print checkpoint/recovery metadata JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if !readyPoolLeasePattern.MatchString(*id) || !*jsonOut {
		return Exit(2, "project-checkpoint requires --id and --json")
	}
	var input *ProjectCheckpointRequest
	if *requestStdin {
		input = &ProjectCheckpointRequest{}
		reader := &io.LimitedReader{R: a.input(), N: 16385}
		decoder := json.NewDecoder(reader)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(input); err != nil || decoder.Decode(&struct{}{}) != io.EOF || reader.N <= 0 {
			return Exit(2, "invalid checkpoint request metadata")
		}
		switch input.Action {
		case "bind", "capture", "restore", "record-loss":
		case "discard":
			if !input.ConfirmDiscard {
				return Exit(2, "discard requires explicit confirmDiscard")
			}
		default:
			return Exit(2, "unsupported checkpoint action")
		}
	}
	coord, err := readyPoolCoordinator()
	if err != nil {
		return err
	}
	response, err := coord.ProjectCheckpoint(ctx, *id, input)
	if err != nil {
		return Exit(7, "project checkpoint operation failed; ordinary reclaim remains held until checkpoint acceptance or explicit loss/discard")
	}
	return json.NewEncoder(a.Stdout).Encode(response)
}
