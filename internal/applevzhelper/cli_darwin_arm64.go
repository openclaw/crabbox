//go:build darwin && arm64 && cgo

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
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	signalNotify               = signal.Notify
	signalStop                 = signal.Stop
	prepareInstanceAssetsFunc  = prepareInstanceAssets
	helperExecutable           = os.Executable
	processStartTime           = readProcessStartTime
	writeMetadataFunc          = writeMetadata
	runStartReadyTimeout       = 45 * time.Second
	runStartPollInterval       = 250 * time.Millisecond
	terminateInstanceGraceTime = 20 * time.Second
	terminateInstancePollTime  = 250 * time.Millisecond
)

const (
	inheritedStartupFD       = 3
	startupAuthorizationByte = 0xa5
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
	imageSHA256 := fs.String("image-sha256", "", "expected source image SHA-256")
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
	details, runtimeErr := validateRuntimeConfig(root, strings.TrimSpace(*image), strings.TrimSpace(*imageSHA256))
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
	imageSHA256 := fs.String("image-sha256", "", "expected source image SHA-256")
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
		ImageSHA256:  strings.TrimSpace(*imageSHA256),
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
	logPath := HelperLogPath(root, *name)
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
	startupReader, startupWriter, err := os.Pipe()
	if err != nil {
		_ = os.RemoveAll(instanceRoot)
		return fmt.Errorf("create helper startup pipe: %w", err)
	}
	defer startupWriter.Close()
	cmd := exec.Command(exe, "serve", "--state-root", root, "--name", *name, "--startup-fd", strconv.Itoa(inheritedStartupFD))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{startupReader}
	if err := cmd.Start(); err != nil {
		_ = startupReader.Close()
		_ = os.RemoveAll(instanceRoot)
		return fmt.Errorf("spawn helper daemon: %w", err)
	}
	_ = startupReader.Close()
	inst.PID = cmd.Process.Pid
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	inst, err = authorizeStartedHelper(root, *name, inst, startupWriter)
	if err != nil {
		_ = startupWriter.Close()
		terminateStartedHelper(cmd.Process, inst.PID)
		failure := errors.Join(
			fmt.Errorf("authorize helper daemon startup: %w", err),
			startupDiagnostics(root, *name),
		)
		_ = os.RemoveAll(instanceRoot)
		return failure
	}
	_ = startupWriter.Close()
	readyTimer := time.NewTimer(*readyTimeout)
	defer readyTimer.Stop()
	pollTicker := time.NewTicker(runStartPollInterval)
	defer pollTicker.Stop()
	for {
		current, err := readMetadata(MetadataPath(root, *name))
		if err == nil {
			handled, err := handleStartReadinessMetadata(root, *name, current, inst.PID, cmd.Process, stdout)
			if handled {
				return err
			}
		}
		select {
		case waitErr := <-waitCh:
			exitDetail := error(nil)
			if waitErr != nil {
				exitDetail = fmt.Errorf("helper daemon process: %w", waitErr)
			}
			failure := errors.Join(
				fmt.Errorf("helper daemon exited before the VM reached running state"),
				exitDetail,
				startupDiagnostics(root, *name),
			)
			_ = os.RemoveAll(instanceRoot)
			return failure
		case <-readyTimer.C:
			terminateStartedHelper(cmd.Process, inst.PID)
			failure := errors.Join(
				fmt.Errorf("timed out waiting for helper daemon to report readiness"),
				startupDiagnostics(root, *name),
			)
			_ = os.RemoveAll(instanceRoot)
			return failure
		case <-pollTicker.C:
		}
	}
}

func authorizeStartedHelper(stateRoot, name string, inst Instance, writer io.Writer) (Instance, error) {
	startedAt, err := processStartTime(inst.PID)
	if err != nil {
		return inst, fmt.Errorf("read helper process identity: %w", err)
	}
	inst.PIDStartedAt = startedAt
	inst.Status = StatusStarting
	inst.UpdatedAt = time.Now().UTC()
	if err := writeMetadataFunc(MetadataPath(stateRoot, name), inst); err != nil {
		return inst, err
	}
	if written, err := writer.Write([]byte{startupAuthorizationByte}); err != nil {
		return inst, fmt.Errorf("signal helper startup authorization: %w", err)
	} else if written != 1 {
		return inst, fmt.Errorf("signal helper startup authorization: %w", io.ErrShortWrite)
	}
	return inst, nil
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
		failure := errors.Join(helperStoppedBeforeReadinessError(inst.Status), startupDiagnostics(stateRoot, name))
		if err := os.RemoveAll(InstanceDir(stateRoot, name)); err != nil {
			return true, errors.Join(failure, fmt.Errorf("remove instance directory: %w", err))
		}
		return true, failure
	default:
		return false, nil
	}
}

var errHelperStoppedBeforeReadiness = errors.New("apple-vz helper stopped before reporting readiness")

func helperStoppedBeforeReadinessError(status string) error {
	return fmt.Errorf("%w (status=%s)", errHelperStoppedBeforeReadiness, status)
}

func startupDiagnostics(stateRoot, name string) error {
	parts := make([]string, 0, 2)
	for _, log := range []struct {
		label string
		path  string
	}{
		{label: HelperLogFileName, path: HelperLogPath(stateRoot, name)},
		{label: ConsoleLogFileName, path: ConsoleLogPath(stateRoot, name)},
	} {
		data, err := readTail(log.path, 8*1024)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				parts = append(parts, fmt.Sprintf("%s unavailable: %v", log.label, err))
			}
			continue
		}
		if data != "" {
			parts = append(parts, fmt.Sprintf("%s tail:\n%s", log.label, data))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return fmt.Errorf("startup diagnostics for %s:\n%s", name, strings.Join(parts, "\n"))
}

func readTail(path string, limit int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	offset := info.Size() - limit
	if offset < 0 {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
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
	startupFD := fs.Int("startup-fd", -1, "inherited startup authorization file descriptor")
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
	if *startupFD >= 0 {
		if err := waitForStartupAuthorization(*startupFD); err != nil {
			return err
		}
	}
	return runServe(root, strings.TrimSpace(*name), stdout, stderr)
}

func waitForStartupAuthorization(fd int) error {
	file := os.NewFile(uintptr(fd), "apple-vz-startup")
	if file == nil {
		return fmt.Errorf("invalid helper startup file descriptor %d", fd)
	}
	defer file.Close()
	var marker [1]byte
	if _, err := io.ReadFull(file, marker[:]); err != nil {
		return fmt.Errorf("helper startup authorization closed before approval: %w", err)
	}
	if marker[0] != startupAuthorizationByte {
		return fmt.Errorf("invalid helper startup authorization marker")
	}
	return nil
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
			instanceRoot := InstanceDir(root, *name)
			if _, statErr := os.Stat(instanceRoot); statErr != nil {
				if errors.Is(statErr, os.ErrNotExist) {
					return json.NewEncoder(stdout).Encode(DeleteResponse{Deleted: false})
				}
				return statErr
			}
			if err := os.RemoveAll(instanceRoot); err != nil {
				return fmt.Errorf("remove metadata-less instance directory: %w", err)
			}
			return json.NewEncoder(stdout).Encode(DeleteResponse{
				Deleted:  true,
				Instance: Instance{Name: strings.TrimSpace(*name), Status: StatusStopped},
			})
		}
		return err
	}
	inst = normalizeInstance(inst)
	if err := terminateInstance(root, *name, inst); err != nil {
		return err
	}
	inst.Status = StatusStopped
	inst.UpdatedAt = time.Now().UTC()
	inst.PID = 0
	inst.PIDStartedAt = ""
	return json.NewEncoder(stdout).Encode(DeleteResponse{Deleted: true, Instance: inst})
}

func terminateInstance(stateRoot, name string, inst Instance) error {
	pid := inst.PID
	if pid > 0 && pidAlive(pid) {
		if processIdentityMatches(inst) {
			process, err := os.FindProcess(pid)
			if err != nil {
				return fmt.Errorf("find helper process %d: %w", pid, err)
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
	}
	if err := os.RemoveAll(InstanceDir(stateRoot, name)); err != nil {
		return fmt.Errorf("remove instance directory: %w", err)
	}
	return nil
}

func processIdentityMatches(inst Instance) bool {
	expected := strings.TrimSpace(inst.PIDStartedAt)
	if inst.PID <= 0 || expected == "" {
		return false
	}
	actual, err := processStartTime(inst.PID)
	return err == nil && actual == expected
}

func readProcessStartTime(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("pid must be positive")
	}
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	startedAt := strings.TrimSpace(string(out))
	if startedAt == "" {
		return "", fmt.Errorf("process %d start time unavailable", pid)
	}
	return startedAt, nil
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
	if inst.PID > 0 && (!pidAlive(inst.PID) || processIdentityChanged(inst)) {
		if IsRunningStatus(inst.Status) || inst.Status == StatusStopping {
			inst.Status = StatusStopped
			inst.PID = 0
			inst.PIDStartedAt = ""
		}
	}
	return inst
}

func processIdentityChanged(inst Instance) bool {
	expected := strings.TrimSpace(inst.PIDStartedAt)
	if inst.PID <= 0 || expected == "" {
		return false
	}
	actual, err := processStartTime(inst.PID)
	return err == nil && actual != expected
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
