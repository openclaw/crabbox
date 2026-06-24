package cua

import "context"

type backend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b backend) Spec() ProviderSpec { return b.spec }

func (b backend) Warmup(context.Context, WarmupRequest) error {
	return planBoundLifecycleError("warmup")
}

func (b backend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{Provider: providerName}, planBoundLifecycleError("run")
}

func (b backend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, planBoundLifecycleError("list")
}

func (b backend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{Provider: providerName}, planBoundLifecycleError("status")
}

func (b backend) Stop(context.Context, StopRequest) error {
	return planBoundLifecycleError("stop")
}

func (b backend) Cleanup(context.Context, CleanupRequest) error {
	return planBoundLifecycleError("cleanup")
}

func planBoundLifecycleError(command string) error {
	return exit(2, "provider=cua %s lifecycle is deferred to PLAN-03; mutation=false", command)
}
