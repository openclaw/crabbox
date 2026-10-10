package exedev

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func validateExeDevBase(cfg core.Config) error {
	base := strings.TrimSpace(cfg.ExeDev.Base)
	if base == "" {
		return nil
	}
	if strings.HasPrefix(base, "-") || strings.ContainsAny(base, "\x00\r\n") {
		return core.Exit(2, "exe.dev base must be a VM name, not an option or multiline value")
	}
	if strings.TrimSpace(cfg.ExeDev.Image) != "" || strings.TrimSpace(cfg.ExeDev.Command) != "" {
		return core.Exit(2, "--exe-dev-from cannot be combined with exe.dev image or command settings; the clone inherits them from its base")
	}
	return nil
}

func (b *exeDevLeaseBackend) cloneVM(ctx context.Context, cfg core.Config, name, leaseID, slug, generation string) (exeDevVM, bool, error) {
	if err := validateExeDevBase(cfg); err != nil {
		return exeDevVM{}, false, err
	}
	args := []string{"cp", strings.TrimSpace(cfg.ExeDev.Base), name, "--copy-tags=false", "--json"}
	if cfg.ExeDev.CPUs > 0 {
		args = append(args, "--cpu", strconv.Itoa(cfg.ExeDev.CPUs))
	}
	if memory := strings.TrimSpace(cfg.ExeDev.Memory); memory != "" {
		args = append(args, "--memory", memory)
	}
	if disk := strings.TrimSpace(cfg.ExeDev.Disk); disk != "" {
		args = append(args, "--disk", disk)
	}
	out, err := b.controlOutput(ctx, args)
	if err != nil {
		// Submission may be ambiguous. Without the fresh generation tag we
		// cannot establish custody from a name alone, even if inventory has it.
		return exeDevVM{}, false, fmt.Errorf("%w; exe.dev copy may have created %s; inspect it before manual cleanup: %s", err, name, b.manualDeleteCommand(name))
	}
	uncertain := func(err error) (exeDevVM, bool, error) {
		return exeDevVM{}, true, fmt.Errorf("%w; exe.dev clone %s could not be verified; inspect it before manual cleanup: %s", err, name, b.manualDeleteCommand(name))
	}
	if !json.Valid([]byte(out)) || !strings.HasPrefix(strings.TrimSpace(out), "{") {
		return uncertain(core.Exit(5, "exe.dev cp returned an invalid JSON object"))
	}
	response, err := parseExeDevVM(out)
	if err != nil {
		return uncertain(core.Exit(5, "exe.dev cp returned an invalid VM response: %v", err))
	}
	if response.Name() != "" && response.Name() != name {
		return uncertain(core.Exit(5, "exe.dev cp returned unexpected VM %s", response.Name()))
	}
	// Check the destination before tagging, including acknowledgement-only
	// responses. The control API is name-based; never retag the response's VM.
	vm, err := b.waitForExeDevVM(ctx, name, core.BootstrapWaitTimeout(cfg), false)
	if err != nil {
		return uncertain(err)
	}
	if len(vm.Tags) != 0 {
		return uncertain(core.Exit(2, "exe.dev clone %s unexpectedly has tags before ownership was assigned", name))
	}
	tags := []string{"crabbox", "crabbox-lease-" + leaseID, "crabbox-slug-" + slug, exeDevClaimGenerationTagPrefix + generation}
	_, tagErr := b.controlOutput(ctx, append([]string{"tag", "--json", name}, tags...))
	vm, err = b.waitForExeDevVM(ctx, name, core.BootstrapWaitTimeout(cfg), false)
	if err == nil {
		err = validateExeDevVMOwnership(vm, leaseID, slug, "clone acquisition")
	}
	if err == nil {
		err = validateExeDevClaimGeneration(vm, generation)
	}
	if err != nil {
		if tagErr != nil {
			err = fmt.Errorf("%w; tagging failed: %v", err, tagErr)
		}
		return uncertain(err)
	}
	if vm.SSHHost() == "" {
		vm, err = b.waitForExeDevSSHRoute(ctx, name, core.BootstrapWaitTimeout(cfg))
		if err == nil {
			err = validateExeDevVMOwnership(vm, leaseID, slug, "clone acquisition")
		}
		if err == nil {
			err = validateExeDevClaimGeneration(vm, generation)
		}
		if err != nil {
			return uncertain(err)
		}
	}
	// A lost tag response is harmless only when inventory proves the exact
	// fresh identity on the VM whose advertised route we will use.
	return vm, true, nil
}
