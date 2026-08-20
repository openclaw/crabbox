package shared

import (
	"context"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type DelegatedStatusResource struct {
	State, ServerID, ServerType string
	Ready                       bool
	Labels                      map[string]string
}

type DelegatedStatusRequest struct {
	ID, Provider, TargetOS string
	Network                core.NetworkMode
	Wait                   bool
	WaitTimeout            time.Duration
	Now                    func() time.Time
	Resolve                func(string) (string, string, string, error)
	Get                    func(context.Context, string) (DelegatedStatusResource, error)
	TimeoutError           func(string) error
}

func PollDelegatedStatus(ctx context.Context, req DelegatedStatusRequest) (core.StatusView, error) {
	leaseID, resourceID, slug, err := req.Resolve(req.ID)
	if err != nil {
		return core.StatusView{}, err
	}
	deadline := req.Now().Add(req.WaitTimeout)
	if req.WaitTimeout <= 0 {
		deadline = req.Now().Add(5 * time.Minute)
	}
	for {
		resource, err := req.Get(ctx, resourceID)
		if err != nil {
			return core.StatusView{}, err
		}
		view := core.StatusView{
			ID:         leaseID,
			Slug:       core.Blank(slug, resource.Labels["slug"]),
			Provider:   req.Provider,
			TargetOS:   req.TargetOS,
			State:      resource.State,
			ServerID:   resource.ServerID,
			ServerType: resource.ServerType,
			Network:    req.Network,
			Ready:      resource.Ready,
			Labels:     resource.Labels,
		}
		if !req.Wait || view.Ready {
			return view, nil
		}
		if req.Now().After(deadline) {
			return core.StatusView{}, req.TimeoutError(resourceID)
		}
		select {
		case <-ctx.Done():
			return core.StatusView{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
