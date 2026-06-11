//go:build darwin && arm64

package applevzhelper

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

var (
	signalNotify               = signal.Notify
	signalStop                 = signal.Stop
	prepareInstanceAssetsFunc  = prepareInstanceAssets
	helperExecutable           = os.Executable
	runStartReadyTimeout       = 45 * time.Second
	runStartPollInterval       = 250 * time.Millisecond
	terminateInstanceGraceTime = 20 * time.Second
	terminateInstancePollTime  = 250 * time.Millisecond
)

func RunCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: crabbox-apple-vz-helper <doctor|start|serve|list|inspect|delete>")
		return 2
	}
	var err error
	switch args[0] {
	case "doctor":
		err = runDoctor(args[1:], stdout, stderr)
	case "start":
		err = runStart(args[1:], stdout, stderr)
	case "serve":
		err = runServeCommand(args[1:], stdout, stderr)
	case "list":
		err = runList(args[1:], stdout, stderr)
	case "inspect":
		err = runInspect(args[1:], stdout, stderr)
	case "delete":
		err = runDelete(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runDoctor(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateRoot := fs.String("state-root", "", "apple-vz state root")
	image := fs.String("image", "", "source image")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := normalizeStateRoot(*stateRoot)
	if err != nil {
		return err
	}
	instances, err := listMetadata(root)
	if err != nil {
		return err
	}
	details, runtimeErr := validateRuntimeConfig(root, strings.TrimSpace(*image))
	if runtimeErr != nil {
		if err := json.NewEncoder(stdout).Encode(DoctorResponse{
			Status:    "error",
			Message:   runtimeErr.Error(),
			Details:   details,
			Instances: len(instances),
		}); err != nil {
			return err
		}
		return runtimeErr
	}
	return json.NewEncoder(stdout).Encode(DoctorResponse{
		Status:    "ok",
		Message:   "runtime ready",
		Details:   details,
		Instances: len(instances),
	})
}

func runStart(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateRoot := fs.String("state-root", "", "apple-vz state root")
	name := fs.String("name", "", "instance name")
	leaseID := fs.String("lease-id", "", "lease id")
	slug := fs.String("slug", "", "lease slug")
	image := fs.String("image", "", "source image")
	sshUser := fs.String("ssh-user", "", "ssh user")
	sshPublicKey := fs.String("ssh-public-key", "", "ssh public key")
	workRoot := fs.String("work-root", "", "work root")
	cpus := fs.Int("cpus", 0, "cpu count")
	memoryMiB := fs.Int("memory-mib", 0, "memory in MiB")
	diskGiB := fs.Int("disk-gib", 0, "disk size in GiB")
	readyTimeout := fs.Duration("ready-timeout", runStartReadyTimeout, "maximum time to wait for helper daemon readiness")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := normalizeStateRoot(*stateRoot)
	if err != nil {
		return err
	}
	if err := validateInstanceName(*name); err != nil {
		return err
	}
	if strings.TrimSpace(*image) == "" {
		return fmt.Errorf("image is required")
	}
	if strings.TrimSpace(*sshUser) == "" {
		return fmt.Errorf("ssh user is required")
	}
	if strings.TrimSpace(*sshPublicKey) == "" {
		return fmt.Errorf("ssh public key is required")
	}
	if strings.TrimSpace(*workRoot) == "" {
		return fmt.Errorf("work root is required")
	}
	if *cpus <= 0 || *memoryMiB <= 0 || *diskGiB <= 0 {
		return fmt.Errorf("cpus, memory-mib, and disk-gib must be positive")
	}
	if *readyTimeout <= 0 {
		return fmt.Errorf("ready-timeout must be positive")
	}
	instanceRoot := InstanceDir(root, *name)
	if existing, err := readMetadata(MetadataPath(root, *name)); err == nil {
		existing = normalizeInstance(existing)
		if IsRunningStatus(existing.Status) {
			return fmt.Errorf("instance %s is already active", *name)
		}
		if err := os.RemoveAll(instanceRoot); err != nil {
			return fmt.Errorf("remove stale instance directory: %w", err)
		}
	}
	if err := os.MkdirAll(instanceRoot, 0o755); err != nil {
		return fmt.Errorf("create instance root: %w", err)
	}
	now := time.Now().UTC()
	inst := Instance{
		Name:      strings.TrimSpace(*name),
		LeaseID:   strings.TrimSpace(*leaseID),
		Slug:      strings.TrimSpace(*slug),
		Status:    StatusStarting,
		Image:     strings.TrimSpace(*image),
		SSHUser:   strings.TrimSpace(*sshUser),
		WorkRoot:  strings.TrimSpace(*workRoot),
		CPUs:      *cpus,
		MemoryMiB: *memoryMiB,
		DiskGiB:   *diskGiB,
		CreatedAt: now,
		UpdatedAt: now,
	}
	inst, err = prepareInstanceAssetsFunc(context.Background(), startConfig{
		StateRoot:    root,
		Instance:     inst,
		SSHPublicKey: strings.TrimSpace(*sshPublicKey),
	})
	if err != nil {
		_ = os.RemoveAll(instanceRoot)
		return err
	}
	if err := writeMetadata(MetadataPath(root, *name), inst); err != nil {
		_ = os.RemoveAll(instanceRoot)
		return err
	}
	logPath := filepath.Join(instanceRoot, helperLogFileName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		_ = os.RemoveAll(instanceRoot)
		return fmt.Errorf("open helper log: %w", err)
	}
	defer logFile.Close()
	exe, err := helperExecutable()
	if err != nil {
		_ = os.RemoveAll(instanceRoot)
		return fmt.Errorf("resolve helper executable: %w", err)
	}
	cmd := exec.Command(exe, "serve", "--state-root", root, "--name", *name)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(instanceRoot)
		return fmt.Errorf("spawn helper daemon: %w", err)
	}
	inst.PID = cmd.Process.Pid
	inst.UpdatedAt = time.Now().UTC()
	if err := writeMetadata(MetadataPath(root, *name), inst); err != nil {
		return err
	}
	deadline := time.Now().Add(*readyTimeout)
	for time.Now().Before(deadline) {
		current, err := readMetadata(MetadataPath(root, *name))
		if err == nil {
			handled, err := handleStartReadinessMetadata(root, *name, current, inst.PID, cmd.Process, stdout)
			if handled {
				if shouldReleaseStartedHelper(err) {
					_ = cmd.Process.Release()
				} else {
					_ = cmd.Wait()
				}
				return err
			}
		}
		if !pidAlive(inst.PID) {
			_ = cmd.Process.Release()
			return fmt.Errorf("helper daemon exited before the VM reached running state")
		}
		time.Sleep(runStartPollInterval)
	}
	_ = terminateInstance(root, *name, inst.PID)
	_ = cmd.Wait()
	return fmt.Errorf("timed out waiting for helper daemon to report readiness")
}

func handleStartReadinessMetadata(stateRoot, name string, inst Instance, expectedPID int, process *os.Process, stdout io.Writer) (bool, error) {
	rawPID := inst.PID
	inst = normalizeInstance(inst)
	if rawPID != expectedPID {
		return false, nil
	}
	switch inst.Status {
	case StatusRunning:
		return true, json.NewEncoder(stdout).Encode(StartResponse{Instance: inst})
	case StatusError:
		return true, errors.New(inst.Error)
	case StatusStopping, StatusStopped:
		terminateStartedHelper(process, expectedPID)
		if err := os.RemoveAll(InstanceDir(stateRoot, name)); err != nil {
			return true, fmt.Errorf("%w; remove instance directory: %v", helperStoppedBeforeReadinessError(inst.Status), err)
		}
		return true, helperStoppedBeforeReadinessError(inst.Status)
	default:
		return false, nil
	}
}

func shouldReleaseStartedHelper(err error) bool {
	if err == nil {
		return true
	}
	return !errors.Is(err, errHelperStoppedBeforeReadiness)
}

var errHelperStoppedBeforeReadiness = errors.New("apple-vz helper stopped before reporting readiness")

func helperStoppedBeforeReadinessError(status string) error {
	return fmt.Errorf("%w (status=%s)", errHelperStoppedBeforeReadiness, status)
}

func terminateStartedHelper(process *os.Process, pid int) {
	if process == nil || pid <= 0 || !pidAlive(pid) {
		return
	}
	_ = process.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(terminateInstanceGraceTime)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(terminateInstancePollTime)
	}
	if pidAlive(pid) {
		_ = process.Signal(syscall.SIGKILL)
	}
}

func runServeCommand(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateRoot := fs.String("state-root", "", "apple-vz state root")
	name := fs.String("name", "", "instance name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := normalizeStateRoot(*stateRoot)
	if err != nil {
		return err
	}
	if err := validateInstanceName(*name); err != nil {
		return err
	}
	return runServe(root, strings.TrimSpace(*name), stdout, stderr)
}

func runList(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateRoot := fs.String("state-root", "", "apple-vz state root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := normalizeStateRoot(*stateRoot)
	if err != nil {
		return err
	}
	instances, err := listMetadata(root)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(ListResponse{Instances: instances})
}

func runInspect(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateRoot := fs.String("state-root", "", "apple-vz state root")
	name := fs.String("name", "", "instance name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := normalizeStateRoot(*stateRoot)
	if err != nil {
		return err
	}
	if err := validateInstanceName(*name); err != nil {
		return err
	}
	inst, err := readMetadata(MetadataPath(root, *name))
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(InspectResponse{Instance: normalizeInstance(inst)})
}

func runDelete(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateRoot := fs.String("state-root", "", "apple-vz state root")
	name := fs.String("name", "", "instance name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := normalizeStateRoot(*stateRoot)
	if err != nil {
		return err
	}
	if err := validateInstanceName(*name); err != nil {
		return err
	}
	inst, err := readMetadata(MetadataPath(root, *name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return json.NewEncoder(stdout).Encode(DeleteResponse{Deleted: false})
		}
		return err
	}
	inst = normalizeInstance(inst)
	if err := terminateInstance(root, *name, inst.PID); err != nil {
		return err
	}
	inst.Status = StatusStopped
	inst.UpdatedAt = time.Now().UTC()
	inst.PID = 0
	return json.NewEncoder(stdout).Encode(DeleteResponse{Deleted: true, Instance: inst})
}

func terminateInstance(stateRoot, name string, pid int) error {
	if pid > 0 && pidAlive(pid) {
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Signal(syscall.SIGTERM)
			deadline := time.Now().Add(terminateInstanceGraceTime)
			for pidAlive(pid) && time.Now().Before(deadline) {
				time.Sleep(terminateInstancePollTime)
			}
			if pidAlive(pid) {
				_ = process.Signal(syscall.SIGKILL)
			}
		}
	}
	if err := os.RemoveAll(InstanceDir(stateRoot, name)); err != nil {
		return fmt.Errorf("remove instance directory: %w", err)
	}
	return nil
}

func normalizeStateRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("state root is required")
	}
	if !filepath.IsAbs(root) {
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", err
		}
		root = abs
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create state root: %w", err)
	}
	return root, nil
}

func validateInstanceName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("instance name is required")
	}
	if name == "." || name == ".." || strings.Contains(name, string(os.PathSeparator)) {
		return fmt.Errorf("invalid instance name %q", name)
	}
	return nil
}

func writeMetadata(path string, inst Instance) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create metadata directory: %w", err)
	}
	inst.UpdatedAt = inst.UpdatedAt.UTC()
	if inst.CreatedAt.IsZero() {
		inst.CreatedAt = inst.UpdatedAt
	}
	data, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return fmt.Errorf("encode metadata: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("commit metadata: %w", err)
	}
	return nil
}

func readMetadata(path string) (Instance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Instance{}, err
	}
	var inst Instance
	if err := json.Unmarshal(data, &inst); err != nil {
		return Instance{}, fmt.Errorf("decode metadata %s: %w", path, err)
	}
	return inst, nil
}

func listMetadata(stateRoot string) ([]Instance, error) {
	entries, err := os.ReadDir(InstancesDir(stateRoot))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	instances := make([]Instance, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		inst, err := readMetadata(filepath.Join(InstancesDir(stateRoot), entry.Name(), MetadataFileName))
		if err != nil {
			continue
		}
		instances = append(instances, normalizeInstance(inst))
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].Name < instances[j].Name })
	return instances, nil
}

func normalizeInstance(inst Instance) Instance {
	if inst.SSHHost == "" && inst.SSHPort > 0 {
		inst.SSHHost = "127.0.0.1"
	}
	if inst.PID > 0 && !pidAlive(inst.PID) {
		if IsRunningStatus(inst.Status) || inst.Status == StatusStopping {
			inst.Status = StatusStopped
			inst.PID = 0
		}
	}
	return inst
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
