package koyeb

import (
	"context"

	core "github.com/openclaw/crabbox/internal/cli"
)

type backend struct {
	spec core.ProviderSpec
}

var (
	_ core.SSHLeaseBackend = (*backend)(nil)
	_ core.CleanupBackend  = (*backend)(nil)
	_ core.DoctorBackend   = (*backend)(nil)
)

func newBackend(spec core.ProviderSpec) *backend {
	return &backend{spec: spec}
}

func (b *backend) Spec() core.ProviderSpec { return b.spec }

func (*backend) Acquire(context.Context, core.AcquireRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, coordinatorRequiredError()
}

func (*backend) Resolve(context.Context, core.ResolveRequest) (core.LeaseTarget, error) {
	return core.LeaseTarget{}, coordinatorRequiredError()
}

func (*backend) List(context.Context, core.ListRequest) ([]core.LeaseView, error) {
	return nil, coordinatorRequiredError()
}

func (*backend) ReleaseLease(context.Context, core.ReleaseLeaseRequest) error {
	return coordinatorRequiredError()
}

func (*backend) Touch(context.Context, core.TouchRequest) (core.Server, error) {
	return core.Server{}, coordinatorRequiredError()
}

func (*backend) Cleanup(context.Context, core.CleanupRequest) error {
	return coordinatorRequiredError()
}

func (*backend) Doctor(context.Context, core.DoctorRequest) (core.DoctorResult, error) {
	return core.DoctorResult{}, coordinatorRequiredError()
}

func coordinatorRequiredError() error {
	return core.Exit(2, "provider=%s requires a configured coordinator; direct lifecycle is not supported", providerName)
}
