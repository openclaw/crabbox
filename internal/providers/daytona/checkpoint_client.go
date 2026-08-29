package daytona

import (
	"context"

	api "github.com/daytonaio/daytona/libs/api-client-go"
)

type daytonaSnapshotAPI interface {
	daytonaAPI
	StopSandbox(context.Context, string) error
	CreateSnapshot(context.Context, string, string) error
	GetSnapshot(context.Context, string) (*api.SnapshotDto, error)
	DeleteSnapshot(context.Context, string) error
}

func (c *daytonaSDKClient) StopSandbox(ctx context.Context, id string) error {
	req := c.api.SandboxAPI.StopSandbox(c.ctx(ctx), id)
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	_, _, err := req.Execute()
	return c.redactError(err)
}

func (c *daytonaSDKClient) CreateSnapshot(ctx context.Context, id, name string) error {
	req := c.api.SandboxAPI.CreateSandboxSnapshot(c.ctx(ctx), id).CreateSandboxSnapshot(*api.NewCreateSandboxSnapshot(name))
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	_, _, err := req.Execute()
	return c.redactError(err)
}

func (c *daytonaSDKClient) GetSnapshot(ctx context.Context, id string) (*api.SnapshotDto, error) {
	req := c.api.SnapshotsAPI.GetSnapshot(c.ctx(ctx), id)
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	out, _, err := req.Execute()
	return out, c.redactError(err)
}

func (c *daytonaSDKClient) DeleteSnapshot(ctx context.Context, id string) error {
	req := c.api.SnapshotsAPI.RemoveSnapshot(c.ctx(ctx), id)
	if c.orgID != "" {
		req = req.XDaytonaOrganizationID(c.orgID)
	}
	_, err := req.Execute()
	return c.redactError(err)
}
