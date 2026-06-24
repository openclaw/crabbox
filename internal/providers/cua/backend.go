package cua

import "context"

type backend struct {
	spec ProviderSpec
	cfg  Config
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

func (b backend) Doctor(context.Context, DoctorRequest) (DoctorResult, error) {
	checks := []DoctorCheck{
		{
			Status:  "ok",
			Check:   "config",
			Message: "provider=cua config=ready mutation=false",
			Details: map[string]string{"provider": providerName, "mutation": "false"},
		},
		{
			Status:  "warning",
			Check:   "bridge",
			Message: "provider=cua bridge=deferred plan=PLAN-02 mutation=false",
			Details: map[string]string{"provider": providerName, "plan": "PLAN-02", "mutation": "false"},
		},
	}
	return DoctorResult{
		Provider: providerName,
		Status:   "warning",
		Message:  "config=ready bridge=deferred plan=PLAN-02 mutation=false",
		Checks:   checks,
	}, nil
}

func planBoundLifecycleError(command string) error {
	return exit(2, "provider=cua %s lifecycle is deferred to PLAN-03; mutation=false", command)
}
