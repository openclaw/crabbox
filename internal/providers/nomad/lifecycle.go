package nomad

import "context"

func (b *backend) Warmup(context.Context, WarmupRequest) error {
	return unsupportedLifecycle("warmup")
}

func (b *backend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{Provider: providerName}, unsupportedLifecycle("run")
}

func (b *backend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, unsupportedLifecycle("list")
}

func (b *backend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{Provider: providerName}, unsupportedLifecycle("status")
}

func (b *backend) Stop(context.Context, StopRequest) error {
	return unsupportedLifecycle("stop")
}

func unsupportedLifecycle(command string) error {
	return exit(2, "nomad %s is not implemented in the Wave 1 config/client foundation", command)
}
