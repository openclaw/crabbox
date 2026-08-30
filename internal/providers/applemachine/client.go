package applemachine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
	result, err := b.control(ctx, []string{"machine", "inspect", name})
	if err != nil {
		return machine{}, exit(4, "Apple container machine %q not found: %s", name, failureDetail(result, err))
	}
	var machines []machine
	if err := json.Unmarshal([]byte(result.Stdout), &machines); err != nil || len(machines) != 1 || machines[0].ID != name || machines[0].Status == "" {
		return machine{}, exit(5, "invalid Apple container machine inspection for %q", name)
	}
	return machines[0], nil
}

func (b *backend) listMachines(ctx context.Context) ([]machine, error) {
	result, err := b.control(ctx, []string{"machine", "list", "--format", "json"})
	if err != nil {
		return nil, exit(5, "list Apple container machines: %s", failureDetail(result, err))
	}
	var machines []machine
	if err := json.Unmarshal([]byte(result.Stdout), &machines); err != nil {
		return nil, exit(5, "decode Apple container machine list: %v", err)
	}
	if machines == nil {
		return nil, exit(5, "Apple container machine list must be a JSON array")
	}
	seen := map[string]bool{}
	for _, item := range machines {
		if !validMachineName(item.ID) || item.Status == "" || seen[item.ID] {
			return nil, exit(5, "invalid or duplicate Apple container machine inventory entry")
		}
		seen[item.ID] = true
	}
	return machines, nil
}

func (b *backend) removeMachine(ctx context.Context, name string) error {
	result, err := b.control(ctx, []string{"machine", "rm", name})
	if err != nil {
		return exit(5, "delete Apple container machine %q: %s", name, failureDetail(result, err))
	}
	return nil
}

// Control responses are private, bounded, and never accepted after a partial failure.
func (b *backend) control(ctx context.Context, args []string) (LocalCommandResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	const limit = 1024 * 1024
	result, err := b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name: blank(strings.TrimSpace(b.cfg.AppleContainer.CLIPath), "container"), Args: args,
		MaxCapturedOutputBytes: limit, CancelGracePeriod: time.Second,
	})
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if len(result.Stdout) > limit || len(result.Stderr) > limit {
		return LocalCommandResult{}, fmt.Errorf("Apple container control output exceeded its limit")
	}
	if err == nil && result.ExitCode != 0 {
		err = fmt.Errorf("Apple container exited with code %d", result.ExitCode)
	}
	return result, err
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
