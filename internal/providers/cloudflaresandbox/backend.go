package cloudflaresandbox

import (
	"context"
	"fmt"
)

type backend struct {
	spec      ProviderSpec
	cfg       Config
	rt        Runtime
	newClient func(Config, Runtime) (bridgeClient, error)
}

func NewBackend(spec ProviderSpec, cfg Config, rt Runtime) Backend {
	cfg.Provider = providerName
	return &backend{spec: spec, cfg: cfg, rt: rt, newClient: newBridgeClient}
}

func (b *backend) Spec() ProviderSpec { return b.spec }

func (b *backend) client() (bridgeClient, error) {
	if b.newClient != nil {
		return b.newClient(b.cfg, b.rt)
	}
	return newBridgeClient(b.cfg, b.rt)
}

func (b *backend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	api, err := b.client()
	if err != nil {
		return DoctorResult{}, err
	}
	checks := []DoctorCheck{}
	health, err := api.Health(ctx)
	if err != nil {
		checks = append(checks, DoctorCheck{Status: "failed", Check: "health", Message: redactSecrets(err.Error()), Details: map[string]string{"mutation": "false"}})
	} else {
		checks = append(checks, DoctorCheck{Status: "ok", Check: "health", Message: fmt.Sprintf("bridge=ready ok=%t mutation=false", health.OK), Details: map[string]string{"mutation": "false"}})
	}
	openapi, err := api.OpenAPI(ctx)
	if err != nil {
		checks = append(checks, DoctorCheck{Status: "warning", Check: "openapi", Message: redactSecrets(err.Error()), Details: map[string]string{"mutation": "false"}})
	} else {
		checks = append(checks, DoctorCheck{Status: "ok", Check: "openapi", Message: fmt.Sprintf("openapi=ready title=%s mutation=false", blank(openapi.Info.Title, "-")), Details: map[string]string{"mutation": "false"}})
	}
	return DoctorResult{
		Provider: providerName,
		Status:   aggregateStatus(checks),
		Message:  "bridge=checked mutation=false",
		Checks:   checks,
	}, nil
}

func (b *backend) Warmup(context.Context, WarmupRequest) error {
	return unsupported("warmup")
}

func (b *backend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, unsupported("run")
}

func (b *backend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, unsupported("list")
}

func (b *backend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, unsupported("status")
}

func (b *backend) Stop(context.Context, StopRequest) error {
	return unsupported("stop")
}

func (b *backend) Cleanup(context.Context, CleanupRequest) error {
	return unsupported("cleanup")
}

func unsupported(method string) error {
	return exit(2, "provider=%s %s is not implemented in foundation; runtime behavior is added by Plan 02", providerName, method)
}

func aggregateStatus(checks []DoctorCheck) string {
	for _, check := range checks {
		if check.Status == "failed" {
			return "failed"
		}
	}
	for _, check := range checks {
		if check.Status == "warning" {
			return "warning"
		}
	}
	return "ok"
}

func blank(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
