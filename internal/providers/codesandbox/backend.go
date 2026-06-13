package codesandbox

import (
	"context"
	"fmt"
)

type codeSandboxBackend struct {
	spec ProviderSpec
	cfg  Config
	rt   Runtime
}

func (b *codeSandboxBackend) Spec() ProviderSpec { return b.spec }

func (b *codeSandboxBackend) Warmup(context.Context, WarmupRequest) error {
	return exit(2, "provider=codesandbox lifecycle warmup is deferred to the lifecycle implementation")
}

func (b *codeSandboxBackend) Run(context.Context, RunRequest) (RunResult, error) {
	return RunResult{}, exit(2, "provider=codesandbox run is deferred to the lifecycle implementation")
}

func (b *codeSandboxBackend) List(context.Context, ListRequest) ([]LeaseView, error) {
	return nil, exit(2, "provider=codesandbox list is deferred to the lifecycle implementation")
}

func (b *codeSandboxBackend) Status(context.Context, StatusRequest) (StatusView, error) {
	return StatusView{}, exit(2, "provider=codesandbox status is deferred to the lifecycle implementation")
}

func (b *codeSandboxBackend) Stop(context.Context, StopRequest) error {
	return exit(2, "provider=codesandbox stop is deferred to the lifecycle implementation")
}

func (b *codeSandboxBackend) Doctor(ctx context.Context, _ DoctorRequest) (DoctorResult, error) {
	token, source, ok := authFromEnv()
	if !ok {
		return DoctorResult{}, missingAuthError{}
	}
	client, err := newCodeSandboxClient(b.cfg, b.rt)
	if err != nil {
		return DoctorResult{}, err
	}
	result := DoctorResult{
		Provider: providerName,
		Checks: []DoctorCheck{
			doctorCheck("codesandbox_auth", nil, map[string]string{
				"source":   source,
				"redacted": "true",
			}),
		},
	}
	listed, err := client.ListSandboxes(ctx, ListSandboxesRequest{Limit: doctorListLimit(b.cfg.CodeSandbox)})
	if err != nil {
		err = fmt.Errorf("%s", redactToken(err.Error(), token))
		result.Checks = append(result.Checks, doctorCheck("codesandbox_sandbox_list", err, map[string]string{
			"mutation": "false",
			"limit":    fmt.Sprint(doctorListLimit(b.cfg.CodeSandbox)),
		}))
		result.Status = "error"
		result.Message = "auth=ready control_plane=blocked inventory=blocked api=list mutation=false"
		return result, err
	}
	result.Checks = append(result.Checks, doctorCheck("codesandbox_sandbox_list", nil, map[string]string{
		"mutation":   "false",
		"limit":      fmt.Sprint(doctorListLimit(b.cfg.CodeSandbox)),
		"totalCount": fmt.Sprint(listed.TotalCount),
	}))
	result.Status = "ok"
	result.Message = inventoryDoctorResult(providerName, len(listed.Sandboxes)).Message
	return result, nil
}

var _ interface {
	Warmup(context.Context, WarmupRequest) error
	Run(context.Context, RunRequest) (RunResult, error)
	List(context.Context, ListRequest) ([]LeaseView, error)
	Status(context.Context, StatusRequest) (StatusView, error)
	Stop(context.Context, StopRequest) error
	Doctor(context.Context, DoctorRequest) (DoctorResult, error)
} = (*codeSandboxBackend)(nil)
