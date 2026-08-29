package hetzner

import (
	"context"
	"errors"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type checkpointSourceClient struct {
	fakeHetznerClient
	getErr error
}

func (c *checkpointSourceClient) GetServer(_ context.Context, id int64) (Server, error) {
	return Server{ID: id}, c.getErr
}

func TestCheckpointSourceAbsenceRequiresExactGet404(t *testing.T) {
	for _, tc := range []struct {
		name            string
		err             error
		absent, failure bool
	}{
		{name: "unlabelled existing source"},
		{name: "exact absence", err: core.HetznerHTTPError{StatusCode: 404, Method: "GET", Path: "/servers/123"}, absent: true},
		{name: "wrong resource", err: core.HetznerHTTPError{StatusCode: 404, Method: "GET", Path: "/servers/456"}, failure: true},
		{name: "transport failure", err: errors.New("transport unavailable"), failure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &checkpointSourceClient{getErr: tc.err}
			old := newHetznerClient
			newHetznerClient = func() (hetznerClient, error) { return client, nil }
			t.Cleanup(func() { newHetznerClient = old })
			b := &hetznerLeaseBackend{}
			absent, err := b.CheckpointSourceAbsent(context.Background(), core.CheckpointSourceRequest{Capture: core.NativeCheckpointCapture{SourceID: "123"}})
			if absent != tc.absent || (err != nil) != tc.failure {
				t.Fatalf("absent=%v err=%v", absent, err)
			}
			if len(client.deletedServers) != 0 {
				t.Fatal("absence mutated source")
			}
		})
	}
}
