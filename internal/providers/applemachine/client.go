package applemachine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type machine struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	IPAddress string `json:"ipAddress,omitempty"`
	CPUs      int    `json:"cpus,omitempty"`
	Memory    uint64 `json:"memory,omitempty"`
}

func (b *backend) command(ctx context.Context, args []string, dir string) (LocalCommandResult, error) {
	return b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   blank(strings.TrimSpace(b.cfg.AppleContainer.CLIPath), "container"),
		Args:   args,
		Dir:    dir,
		Stdout: b.rt.Stdout,
		Stderr: b.rt.Stderr,
	})
}

func (b *backend) createMachine(ctx context.Context, name string) error {
	args := []string{"machine", "create", "--name", name, "--home-mount", "rw"}
	if b.cfg.AppleContainer.CPUs > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%d", b.cfg.AppleContainer.CPUs))
	}
	if memory := strings.TrimSpace(b.cfg.AppleContainer.Memory); memory != "" {
		args = append(args, "--memory", memory)
	}
	args = append(args, blank(strings.TrimSpace(b.cfg.AppleContainer.Image), "ubuntu:26.04"))
	result, err := b.command(ctx, args, "")
	if err != nil {
		return exit(5, "create Apple container machine: %s", failureDetail(result, err))
	}
	return nil
}

func (b *backend) inspectMachine(ctx context.Context, name string) (machine, error) {
	result, err := b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name: blank(strings.TrimSpace(b.cfg.AppleContainer.CLIPath), "container"),
		Args: []string{"machine", "inspect", name},
	})
	if err != nil {
		return machine{}, exit(4, "Apple container machine %q not found: %s", name, failureDetail(result, err))
	}
	var machines []machine
	if err := json.Unmarshal([]byte(result.Stdout), &machines); err != nil || len(machines) != 1 {
		return machine{}, exit(5, "decode Apple container machine %q: %v", name, err)
	}
	return machines[0], nil
}

func (b *backend) listMachines(ctx context.Context) ([]machine, error) {
	result, err := b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name: blank(strings.TrimSpace(b.cfg.AppleContainer.CLIPath), "container"),
		Args: []string{"machine", "list", "--format", "json"},
	})
	if err != nil {
		return nil, exit(5, "list Apple container machines: %s", failureDetail(result, err))
	}
	var machines []machine
	if strings.TrimSpace(result.Stdout) == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(result.Stdout), &machines); err != nil {
		return nil, exit(5, "decode Apple container machine list: %v", err)
	}
	return machines, nil
}

func (b *backend) removeMachine(ctx context.Context, name string) error {
	result, err := b.command(ctx, []string{"machine", "rm", name}, "")
	if err != nil {
		return exit(5, "delete Apple container machine %q: %s", name, failureDetail(result, err))
	}
	return nil
}

func failureDetail(result LocalCommandResult, err error) string {
	if detail := strings.TrimSpace(result.Stderr); detail != "" {
		return detail
	}
	if detail := strings.TrimSpace(result.Stdout); detail != "" {
		return detail
	}
	return err.Error()
}
