package machine0

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	maxCLIOutput                    = 16 << 20
	machine0RateLimitMessage        = "rate limited. please wait a moment and try again."
	machine0UnavailableMessage      = "the cloud provider is temporarily unavailable. please try again shortly."
	machine0ReadRetryFallback       = 5 * time.Second
	machine0ReadRetryMinimumCadence = time.Second
)

var machine0UUIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type machine0API interface {
	Version(context.Context) (string, error)
	List(context.Context) ([]machine, error)
	Get(context.Context, string) (machine, error)
	SelectedKey(context.Context, string) (*machineKey, error)
	Create(context.Context, createMachineRequest) error
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Suspend(context.Context, string) error
	Remove(context.Context, string) error
	PrimeSSH(context.Context, string) error
	Sizes(context.Context) ([]machineSize, error)
	ListImages(context.Context) ([]machineImage, error)
	GetImage(context.Context, string) (machineImageDetail, error)
	SaveImage(context.Context, string, string, map[string]string) error
	RemoveImage(context.Context, string) error
	RemoveImageVersion(context.Context, string, int) error
}

type client struct {
	cfg   Machine0Config
	rt    Runtime
	sleep func(context.Context, time.Duration) error
}

type machine struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	URL                string          `json:"url"`
	Status             string          `json:"status"`
	IP                 string          `json:"ip"`
	Size               string          `json:"size"`
	VCPU               int             `json:"vcpu"`
	RAM                int             `json:"ram"`
	Disk               int             `json:"disk"`
	Region             string          `json:"region"`
	Provider           string          `json:"provider"`
	Image              string          `json:"image"`
	ImageVersion       int             `json:"imageVersion"`
	DefaultSSHUsername string          `json:"defaultSSHUsername"`
	Distribution       string          `json:"distribution"`
	Key                *machineKey     `json:"key"`
	AgeMinutes         int64           `json:"ageMinutes"`
	TotalCost          int64           `json:"totalCost"`
	PricePerHour       int64           `json:"pricePerHour"`
	MountPaths         json.RawMessage `json:"mountPaths"`
	LastErrorMessage   string          `json:"lastErrorMessage"`
}

type machineKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	FileName  string `json:"fileName"`
	PublicKey string `json:"publicKey"`
	IsDefault bool   `json:"isDefault"`
}

type createMachineRequest struct {
	Name         string
	Size         string
	Region       string
	Image        string
	ImageVersion int
	Key          string
}

type machineSize struct {
	Size                string          `json:"size"`
	VCPU                int             `json:"vcpu"`
	RAMGB               int             `json:"ramGb"`
	DiskGB              int             `json:"diskGb"`
	GPU                 *machineSizeGPU `json:"gpu"`
	Regions             []string        `json:"regions"`
	PricePerHourMicro   int64           `json:"pricePerHourMicro"`
	TransferGiBPerMonth int64           `json:"transferGibPerMonth"`
	EstimatedSnapshotGB int             `json:"estimatedSnapshotGb"`
	DefaultImage        string          `json:"defaultImage"`
	ProviderMetadata    map[string]any  `json:"-"`
}

type machineSizeGPU struct {
	Label         string `json:"label"`
	VRAMGB        int    `json:"vramGb"`
	ScratchDiskGB int    `json:"scratchDiskGb"`
}

func (s *machineSize) UnmarshalJSON(data []byte) error {
	type alias machineSize
	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var raw map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	for _, key := range []string{"size", "vcpu", "ramGb", "diskGb", "gpu", "regions", "pricePerHourMicro", "transferGibPerMonth", "estimatedSnapshotGb", "defaultImage"} {
		delete(raw, key)
	}
	*s = machineSize(value)
	s.ProviderMetadata = raw
	return nil
}

func (c *client) run(ctx context.Context, args ...string) (LocalCommandResult, error) {
	started := time.Now()
	result, err := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:                   c.cfg.CLIPath,
		Args:                   args,
		MaxCapturedOutputBytes: maxCLIOutput,
	})
	if err == nil && result.ExitCode == 0 {
		return result, nil
	}
	if errors.Is(err, exec.ErrNotFound) || strings.Contains(strings.ToLower(fmt.Sprint(err)), "executable file not found") || strings.Contains(strings.ToLower(fmt.Sprint(err)), "no such file or directory") {
		return result, exit(3, "Machine0 CLI %q was not found; install @machine0/cli and ensure machine0 is on PATH (for example: npm install -g @machine0/cli)", c.cfg.CLIPath)
	}
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail == "" {
		detail = strings.TrimSpace(fmt.Sprint(err))
	}
	if cause := context.Cause(ctx); cause != nil {
		if output := strings.TrimSpace(strings.Join([]string{result.Stderr, result.Stdout}, "\n")); output != "" {
			detail = fmt.Sprintf("%v after %s; partial output: %s", cause, time.Since(started).Round(time.Millisecond), output)
		} else {
			detail = fmt.Sprintf("%v after %s", cause, time.Since(started).Round(time.Millisecond))
		}
	} else if err != nil && result.ExitCode < 0 {
		if output := strings.TrimSpace(strings.Join([]string{result.Stderr, result.Stdout}, "\n")); output != "" {
			detail = fmt.Sprintf("%v; partial output: %s", err, output)
		} else {
			detail = err.Error()
		}
	}
	lower := strings.ToLower(detail)
	if strings.Contains(lower, "not logged in") || strings.Contains(lower, "not authenticated") || strings.Contains(lower, "unauthorized") {
		return result, exit(3, "Machine0 authentication is required; run `machine0 login` or set MACHINE0_API_TOKEN: %s", detail)
	}
	return result, exit(5, "machine0 %s failed: %s", strings.Join(args, " "), blank(detail, "unknown error"))
}

func (c *client) runRead(ctx context.Context, args ...string) (LocalCommandResult, error) {
	delay := machine0ReadRetryDelay(c.cfg.PollInterval)
	sleep := c.sleep
	if sleep == nil {
		sleep = sleepContext
	}
	warned := false
	for {
		if err := context.Cause(ctx); err != nil {
			return LocalCommandResult{}, err
		}
		result, err := c.run(ctx, args...)
		if err == nil {
			return result, nil
		}
		if ctxErr := context.Cause(ctx); ctxErr != nil {
			return result, ctxErr
		}
		if !machine0ReadUnavailable(result, err) {
			return result, err
		}
		if !warned {
			if c.rt.Stderr != nil {
				_, _ = fmt.Fprintf(c.rt.Stderr, "machine0 read unavailable; retrying every %s until the current operation ends\n", delay)
			}
			warned = true
		}
		if err := sleep(ctx, delay); err != nil {
			if ctxErr := context.Cause(ctx); ctxErr != nil {
				return result, ctxErr
			}
			return result, err
		}
	}
}

func machine0ReadRetryDelay(configured time.Duration) time.Duration {
	if configured <= 0 {
		return machine0ReadRetryFallback
	}
	if configured < machine0ReadRetryMinimumCadence {
		return machine0ReadRetryMinimumCadence
	}
	return configured
}

func machine0ReadUnavailable(result LocalCommandResult, err error) bool {
	detail := strings.ToLower(strings.Join([]string{result.Stdout, result.Stderr, fmt.Sprint(err)}, "\n"))
	return strings.Contains(detail, machine0RateLimitMessage) || strings.Contains(detail, machine0UnavailableMessage)
}

func decodeJSON(output string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func (c *client) Version(ctx context.Context) (string, error) {
	result, err := c.run(ctx, "--version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout + result.Stderr), nil
}

func (c *client) List(ctx context.Context) ([]machine, error) {
	result, err := c.runRead(ctx, "ls", "--json")
	if err != nil {
		return nil, err
	}
	var machines []machine
	if err := decodeJSON(result.Stdout, &machines); err != nil {
		return nil, exit(5, "parse machine0 ls --json: %v", err)
	}
	for i := range machines {
		if err := validateMachine(machines[i], false); err != nil {
			return nil, exit(5, "parse machine0 ls --json item %d: %v", i, err)
		}
	}
	return machines, nil
}

func (c *client) Get(ctx context.Context, name string) (machine, error) {
	var id string
	if machine0UUIDPattern.MatchString(name) {
		// Native UUID get uses an unavailable procedure in CLI 1.0.164.
		// Resolve a transport name from inventory, then verify full detail identity.
		id, name = name, ""
		machines, err := c.List(ctx)
		if err != nil {
			return machine{}, err
		}
		if machines == nil {
			return machine{}, exit(5, "invalid machine0 ls --json for UUID lookup: expected an array")
		}
		for i, candidate := range machines {
			if !machine0UUIDPattern.MatchString(candidate.ID) {
				return machine{}, exit(5, "invalid machine0 ls --json item %d: missing or malformed UUID", i)
			}
			if strings.EqualFold(candidate.ID, id) {
				if name != "" {
					return machine{}, exit(5, "multiple Machine0 inventory entries match UUID %s", id)
				}
				name = candidate.Name
			}
		}
		if name == "" {
			return machine{}, exit(4, "Machine0 UUID %s is absent from current authorized inventory", id)
		}
		if machine0UUIDPattern.MatchString(name) {
			return machine{}, exit(5, "invalid machine name in Machine0 inventory for UUID %s: %q", id, name)
		}
	}
	result, err := c.runRead(ctx, "get", name, "--json")
	if err != nil {
		return machine{}, err
	}
	var item machine
	if err := decodeJSON(result.Stdout, &item); err != nil {
		return machine{}, exit(5, "parse machine0 get %s --json: %v", name, err)
	}
	if err := validateMachine(item, true); err != nil {
		return machine{}, exit(5, "invalid machine0 get %s --json: %v", name, err)
	}
	if id != "" && (!strings.EqualFold(item.ID, id) || item.Name != name) {
		return machine{}, exit(5, "Machine0 lookup identity changed: expected id=%s name=%q, found id=%s name=%q", id, name, item.ID, item.Name)
	}
	return item, nil
}

func (c *client) SelectedKey(ctx context.Context, name string) (*machineKey, error) {
	if name = strings.TrimSpace(name); name != "" {
		result, err := c.runRead(ctx, "keys", "get", name, "--json")
		if err != nil {
			return nil, err
		}
		var key machineKey
		if err := decodeJSON(result.Stdout, &key); err != nil {
			return nil, exit(5, "parse machine0 keys get %s --json: %v", name, err)
		}
		if strings.TrimSpace(key.Name) != name {
			return nil, exit(5, "machine0 key lookup returned mismatched key name: expected %s, found %s", name, blank(key.Name, "<empty>"))
		}
		return &key, nil
	}
	result, err := c.runRead(ctx, "keys", "ls", "--json")
	if err != nil {
		return nil, err
	}
	var keys []machineKey
	if err := decodeJSON(result.Stdout, &keys); err != nil {
		return nil, exit(5, "parse machine0 keys ls --json: %v", err)
	}
	for index := range keys {
		if keys[index].IsDefault {
			return &keys[index], nil
		}
	}
	return nil, nil
}

func validateMachine(item machine, requireID bool) error {
	if strings.TrimSpace(item.Name) == "" {
		return errors.New("missing name")
	}
	if strings.TrimSpace(item.Status) == "" {
		return errors.New("missing status")
	}
	if requireID && strings.TrimSpace(item.ID) == "" {
		return errors.New("missing id")
	}
	return nil
}

func (c *client) Create(ctx context.Context, req createMachineRequest) error {
	args := []string{"new", req.Name, "--size", req.Size, "--region", req.Region, "--image", req.Image}
	if req.ImageVersion > 0 {
		args = append(args, "--image-version", fmt.Sprint(req.ImageVersion))
	}
	if strings.TrimSpace(req.Key) != "" {
		args = append(args, "--key", req.Key)
	}
	_, err := c.run(ctx, args...)
	return err
}

func (c *client) Start(ctx context.Context, name string) error {
	_, err := c.run(ctx, "start", name)
	return err
}

func (c *client) Stop(ctx context.Context, name string) error {
	_, err := c.run(ctx, "stop", name)
	return err
}

func (c *client) Suspend(ctx context.Context, name string) error {
	_, err := c.run(ctx, "suspend", name, "--yes")
	return err
}

func (c *client) Remove(ctx context.Context, name string) error {
	_, err := c.run(ctx, "rm", name, "--yes")
	return err
}

func (c *client) PrimeSSH(ctx context.Context, name string) error {
	_, err := c.run(ctx, "ssh", name, "true")
	return err
}

func (c *client) Sizes(ctx context.Context) ([]machineSize, error) {
	result, err := c.runRead(ctx, "sizes", "--all", "--json")
	if err != nil {
		return nil, err
	}
	var sizes []machineSize
	if err := decodeJSON(result.Stdout, &sizes); err != nil {
		return nil, exit(5, "parse machine0 sizes --json: %v", err)
	}
	for i, size := range sizes {
		if strings.TrimSpace(size.Size) == "" || size.VCPU <= 0 || size.RAMGB <= 0 || size.DiskGB <= 0 || size.PricePerHourMicro < 0 {
			return nil, exit(5, "invalid machine0 size catalog item %d", i)
		}
		if size.GPU != nil && (strings.TrimSpace(size.GPU.Label) == "" || size.GPU.VRAMGB <= 0 || size.GPU.ScratchDiskGB < 0) {
			return nil, exit(5, "invalid machine0 GPU metadata for size %s", size.Size)
		}
	}
	return sizes, nil
}
