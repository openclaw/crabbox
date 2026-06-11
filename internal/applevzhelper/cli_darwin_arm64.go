//go:build darwin && arm64 && cgo

package applevzhelper

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/sys/unix"
)

var (
	signalNotify               = signal.Notify
	signalStop                 = signal.Stop
	prepareInstanceAssetsFunc  = prepareInstanceAssets
	helperExecutable           = os.Executable
	processStartTime           = readProcessStartTime
	processArguments           = readProcessArguments
	processAlive               = pidAlive
	signalProcess              = sendProcessSignal
	writeMetadataFunc          = writeMetadata
	runStartReadyTimeout       = 45 * time.Second
	runStartPollInterval       = 250 * time.Millisecond
	terminateInstanceGraceTime = 20 * time.Second
	terminateInstancePollTime  = 250 * time.Millisecond
	pidlessStartupStaleAfter   = 2 * time.Minute
	metadataLessStaleAfter     = 6 * time.Hour
)

const (
	inheritedStartupFD       = 3
	startupAuthorizationByte = 0xa5
	processIdentityPrefix    = "darwin-kinfo:"
)

type preparationMarker struct {
	PID          int       `json:"pid"`
	PIDStartedAt string    `json:"pidStartedAt"`
	StartedAt    time.Time `json:"startedAt"`
	Instance     Instance  `json:"instance"`
}

func RunCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	useLock, err := lockManagedHelperUse()
	if err != nil {
		fmt.Fprintf(stderr, "lock managed helper use: %v\n", err)
		return 1
	}
	if useLock != nil {
		defer useLock.Unlock()
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: crabbox-apple-vz-helper <doctor|start|serve|list|inspect|delete>")
		return 2
	}
	err = nil
	switch args[0] {
	case "doctor":
		err = runDoctor(args[1:], stdin, stdout, stderr)
	case "start":
		err = runStart(args[1:], stdin, stdout, stderr)
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

func lockManagedHelperUse() (*flock.Flock, error) {
	path := strings.TrimSpace(os.Getenv(ManagedHelperUseLockEnv))
	if path == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%s must be an absolute path", ManagedHelperUseLockEnv)
	}
	lock := flock.New(path, flock.SetPermissions(0o600))
	if err := lock.RLock(); err != nil {
		return nil, err
	}
	return lock, nil
}

func runDoctor(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateRoot := fs.String("state-root", "", "apple-vz state root")
	imageRequestStdin := fs.Bool("image-request-stdin", false, "read source image request as JSON from stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	imageRequest, err := readImageRequest(stdin, *imageRequestStdin)
	if err != nil {
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
	details, runtimeErr := validateRuntimeConfig(root, imageRequest.Image, imageRequest.SHA256)
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

func runStart(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateRoot := fs.String("state-root", "", "apple-vz state root")
	name := fs.String("name", "", "instance name")
	leaseID := fs.String("lease-id", "", "lease id")
	slug := fs.String("slug", "", "lease slug")
	imageRequestStdin := fs.Bool("image-request-stdin", false, "read source image request as JSON from stdin")
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
	imageRequest, err := readImageRequest(stdin, *imageRequestStdin)
	if err != nil {
		return err
	}
	root, err := normalizeStateRoot(*stateRoot)
	if err != nil {
		return err
	}
	if err := validateInstanceName(*name); err != nil {
		return err
	}
	if strings.TrimSpace(*sshUser) == "" {
		return fmt.Errorf("ssh user is required")
	}
	if err := ValidatePOSIXAccountName(*sshUser); err != nil {
		return err
	}
	if strings.TrimSpace(*sshPublicKey) == "" {
		return fmt.Errorf("ssh public key is required")
	}
	if strings.TrimSpace(*workRoot) == "" {
		return fmt.Errorf("work root is required")
	}
	if err := ValidatePOSIXWorkRoot(*workRoot); err != nil {
		return err
	}
	if *cpus <= 0 || *diskGiB <= 0 {
		return fmt.Errorf("cpus and disk-gib must be positive")
	}
	if *memoryMiB < 1024 {
		return fmt.Errorf("memory-mib must be at least 1024")
	}
	if *readyTimeout <= 0 {
		return fmt.Errorf("ready-timeout must be positive")
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
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
	if err := ensurePrivateDir(instanceRoot); err != nil {
		return fmt.Errorf("create instance root: %w", err)
	}
	now := time.Now().UTC()
	inst := Instance{
		Name:      strings.TrimSpace(*name),
		LeaseID:   strings.TrimSpace(*leaseID),
		Slug:      strings.TrimSpace(*slug),
		Status:    StatusStarting,
		Image:     ImageIdentity(imageRequest.Image, imageRequest.SHA256),
		SSHUser:   strings.TrimSpace(*sshUser),
		WorkRoot:  strings.TrimSpace(*workRoot),
		CPUs:      *cpus,
		MemoryMiB: *memoryMiB,
		DiskGiB:   *diskGiB,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := writePreparationMarker(root, *name, inst); err != nil {
		_ = os.RemoveAll(instanceRoot)
		return err
	}
	inst, err = prepareInstanceAssetsFunc(ctx, startConfig{
		StateRoot:    root,
		Instance:     inst,
		Image:        imageRequest.Image,
		ImageSHA256:  imageRequest.SHA256,
		SSHPublicKey: strings.TrimSpace(*sshPublicKey),
	})
	if err != nil {
		_ = os.RemoveAll(instanceRoot)
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(instanceRoot)
		return err
	}
	if err := writeMetadata(MetadataPath(root, *name), inst); err != nil {
		_ = os.RemoveAll(instanceRoot)
		return err
	}
	logPath := HelperLogPath(root, *name)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = os.RemoveAll(instanceRoot)
		return fmt.Errorf("open helper log: %w", err)
	}
	if err := logFile.Chmod(0o600); err != nil {
		logFile.Close()
		_ = os.RemoveAll(instanceRoot)
		return fmt.Errorf("secure helper log: %w", err)
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
	cmd.Env = helperDaemonEnv()
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
		failure := errors.Join(
			fmt.Errorf("authorize helper daemon startup: %w", err),
			startupDiagnostics(root, *name),
			cleanupUnauthorizedStartedHelper(inst, instanceRoot, waitCh),
		)
		return failure
	}
	_ = startupWriter.Close()
	if err := os.Remove(PreparationPath(root, *name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		failure := errors.Join(
			fmt.Errorf("remove preparation marker: %w", err),
			startupDiagnostics(root, *name),
			cleanupStartedHelper(inst, instanceRoot),
		)
		return failure
	}
	readyTimer := time.NewTimer(*readyTimeout)
	defer readyTimer.Stop()
	pollTicker := time.NewTicker(runStartPollInterval)
	defer pollTicker.Stop()
	for {
		current, err := readMetadata(MetadataPath(root, *name))
		if err == nil {
			handled, err := handleStartReadinessMetadata(root, *name, current, inst, stdout)
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
			failure := errors.Join(
				fmt.Errorf("timed out waiting for helper daemon to report readiness"),
				startupDiagnostics(root, *name),
				cleanupStartedHelper(inst, instanceRoot),
			)
			return failure
		case <-ctx.Done():
			return errors.Join(ctx.Err(), cleanupStartedHelper(inst, instanceRoot))
		case <-pollTicker.C:
		}
	}
}

func readImageRequest(stdin io.Reader, enabled bool) (ImageRequest, error) {
	if !enabled {
		return ImageRequest{}, fmt.Errorf("--image-request-stdin is required")
	}
	if stdin == nil {
		return ImageRequest{}, fmt.Errorf("image request stdin is unavailable")
	}
	var request ImageRequest
	if err := json.NewDecoder(io.LimitReader(stdin, 64*1024)).Decode(&request); err != nil {
		return ImageRequest{}, fmt.Errorf("decode image request from stdin: %w", err)
	}
	request.Image = strings.TrimSpace(request.Image)
	request.SHA256 = strings.TrimSpace(request.SHA256)
	if request.Image == "" {
		return ImageRequest{}, fmt.Errorf("image is required")
	}
	return request, nil
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

func handleStartReadinessMetadata(stateRoot, name string, inst, expected Instance, stdout io.Writer) (bool, error) {
	rawPID := inst.PID
	inst = normalizeInstance(inst)
	if rawPID != expected.PID {
		return false, nil
	}
	switch inst.Status {
	case StatusRunning:
		return true, json.NewEncoder(stdout).Encode(StartResponse{Instance: inst})
	case StatusError:
		return true, errors.New(inst.Error)
	case StatusStopping, StatusStopped:
		failure := errors.Join(helperStoppedBeforeReadinessError(inst.Status), startupDiagnostics(stateRoot, name))
		return true, errors.Join(failure, cleanupStartedHelper(expected, InstanceDir(stateRoot, name)))
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
			parts = append(parts, fmt.Sprintf("%s tail:\n%s", log.label, SanitizeDiagnosticText(data)))
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

func cleanupStartedHelper(inst Instance, instanceRoot string) error {
	if err := terminateStartedHelper(inst); err != nil {
		return fmt.Errorf("terminate helper daemon: %w", err)
	}
	if err := os.RemoveAll(instanceRoot); err != nil {
		return fmt.Errorf("remove instance directory: %w", err)
	}
	return nil
}

func cleanupUnauthorizedStartedHelper(inst Instance, instanceRoot string, waitCh <-chan error) error {
	timer := time.NewTimer(terminateInstanceGraceTime)
	defer timer.Stop()
	select {
	case <-waitCh:
		if err := os.RemoveAll(instanceRoot); err != nil {
			return fmt.Errorf("remove instance directory: %w", err)
		}
		return nil
	case <-timer.C:
		if strings.TrimSpace(inst.PIDStartedAt) == "" {
			return fmt.Errorf("helper process %d did not exit after startup authorization closed", inst.PID)
		}
		return cleanupStartedHelper(inst, instanceRoot)
	}
}

func terminateStartedHelper(inst Instance) error {
	matches, err := startedHelperIdentityMatches(inst)
	if err != nil || !matches {
		return err
	}
	if err := signalProcess(inst.PID, syscall.SIGTERM); err != nil && processAlive(inst.PID) {
		return fmt.Errorf("signal helper process %d: %w", inst.PID, err)
	}
	deadline := time.Now().Add(terminateInstanceGraceTime)
	for time.Now().Before(deadline) {
		matches, err := startedHelperIdentityMatches(inst)
		if err != nil || !matches {
			return err
		}
		time.Sleep(terminateInstancePollTime)
	}
	matches, err = startedHelperIdentityMatches(inst)
	if err != nil || !matches {
		return err
	}
	if err := signalProcess(inst.PID, syscall.SIGKILL); err != nil && processAlive(inst.PID) {
		return fmt.Errorf("kill helper process %d: %w", inst.PID, err)
	}
	deadline = time.Now().Add(terminateInstanceGraceTime)
	for time.Now().Before(deadline) {
		matches, err := startedHelperIdentityMatches(inst)
		if err != nil || !matches {
			return err
		}
		time.Sleep(terminateInstancePollTime)
	}
	return fmt.Errorf("helper process %d remained alive after SIGKILL", inst.PID)
}

func startedHelperIdentityMatches(inst Instance) (bool, error) {
	if inst.PID <= 0 || !processAlive(inst.PID) {
		return false, nil
	}
	matches, err := processIdentityMatches(inst)
	if err != nil {
		if processAlive(inst.PID) {
			return false, fmt.Errorf("verify helper process %d identity: %w", inst.PID, err)
		}
		return false, nil
	}
	return matches, nil
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
	inst = migrateLegacyProcessIdentity(inst)
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
	if preparationActive(root, *name) {
		return fmt.Errorf("instance %s is still starting; retry after startup completes", strings.TrimSpace(*name))
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
	inst = migrateLegacyProcessIdentity(inst)
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
	if pid > 0 && processAlive(pid) {
		matches, err := processIdentityMatches(inst)
		if err != nil {
			if processAlive(pid) {
				return fmt.Errorf("verify helper process %d identity: %w", pid, err)
			}
		} else if matches {
			if err := signalProcess(pid, syscall.SIGTERM); err != nil && processAlive(pid) {
				return fmt.Errorf("signal helper process %d: %w", pid, err)
			}
			deadline := time.Now().Add(terminateInstanceGraceTime)
			for processAlive(pid) && time.Now().Before(deadline) {
				matches, err := processIdentityMatches(inst)
				if err != nil {
					if processAlive(pid) {
						return fmt.Errorf("reverify helper process %d identity: %w", pid, err)
					}
					break
				}
				if !matches {
					break
				}
				time.Sleep(terminateInstancePollTime)
			}
			if processAlive(pid) {
				matches, err := processIdentityMatches(inst)
				if err != nil {
					if processAlive(pid) {
						return fmt.Errorf("reverify helper process %d identity before kill: %w", pid, err)
					}
				} else if matches {
					if err := signalProcess(pid, syscall.SIGKILL); err != nil && processAlive(pid) {
						return fmt.Errorf("kill helper process %d: %w", pid, err)
					}
				}
			}
		}
	}
	if err := os.RemoveAll(InstanceDir(stateRoot, name)); err != nil {
		return fmt.Errorf("remove instance directory: %w", err)
	}
	return nil
}

func sendProcessSignal(pid int, signal os.Signal) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(signal)
}

func processIdentityMatches(inst Instance) (bool, error) {
	expected := strings.TrimSpace(inst.PIDStartedAt)
	if inst.PID <= 0 {
		return false, nil
	}
	if expected == "" {
		return false, fmt.Errorf("recorded process start time is missing")
	}
	if strings.ContainsAny(expected, " \t\r\n") {
		return legacyProcessIdentityMatches(inst.PID, expected, "serve", inst.Name)
	}
	actual, err := processStartTime(inst.PID)
	if err != nil {
		return false, err
	}
	return actual == expected, nil
}

func legacyProcessIdentityMatches(pid int, expected, subcommand, name string) (bool, error) {
	actual, err := processStartTime(pid)
	if err != nil {
		return false, err
	}
	actualSeconds, err := kernelProcessIdentitySeconds(actual)
	if err != nil {
		return false, err
	}
	legacySeconds, err := legacyProcessStartSeconds(expected)
	if err != nil {
		return false, err
	}
	if !slices.Contains(legacySeconds, actualSeconds) {
		return false, nil
	}
	args, err := processArguments(pid)
	if err != nil {
		return false, fmt.Errorf("read legacy process arguments: %w", err)
	}
	return helperProcessArgumentsMatch(args, subcommand, name), nil
}

func legacyProcessStartSeconds(value string) ([]int64, error) {
	local, err := systemLocalLocation()
	if err != nil {
		return nil, err
	}
	return legacyProcessStartSecondsIn(value, local)
}

func systemLocalLocation() (*time.Location, error) {
	data, err := os.ReadFile("/etc/localtime")
	if err != nil {
		return nil, fmt.Errorf("read system timezone: %w", err)
	}
	location, err := time.LoadLocationFromTZData("system-local", data)
	if err != nil {
		return nil, fmt.Errorf("load system timezone: %w", err)
	}
	return location, nil
}

func legacyProcessStartSecondsIn(value string, local *time.Location) ([]int64, error) {
	const layout = "Mon Jan _2 15:04:05 2006"
	value = strings.TrimSpace(value)
	locations := []*time.Location{local}
	if local != time.UTC {
		locations = append(locations, time.UTC)
	}
	seconds := make([]int64, 0, len(locations))
	var parseErr error
	for _, location := range locations {
		startedAt, err := time.ParseInLocation(layout, value, location)
		if err != nil {
			parseErr = err
			continue
		}
		if !slices.Contains(seconds, startedAt.Unix()) {
			seconds = append(seconds, startedAt.Unix())
		}
	}
	if len(seconds) == 0 {
		return nil, fmt.Errorf("parse legacy process start time: %w", parseErr)
	}
	return seconds, nil
}

func kernelProcessIdentitySeconds(identity string) (int64, error) {
	identity = strings.TrimSpace(identity)
	if !strings.HasPrefix(identity, processIdentityPrefix) {
		return 0, fmt.Errorf("invalid kernel process identity %q", identity)
	}
	raw := strings.TrimPrefix(identity, processIdentityPrefix)
	parts := strings.Split(raw, ".")
	if len(parts) != 2 || len(parts[1]) != 6 {
		return 0, fmt.Errorf("invalid kernel process identity %q", identity)
	}
	seconds, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("invalid kernel process identity %q", identity)
	}
	microseconds, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || microseconds < 0 || microseconds >= 1_000_000 {
		return 0, fmt.Errorf("invalid kernel process identity %q", identity)
	}
	return seconds, nil
}

func helperProcessArgumentsMatch(args []string, subcommand, name string) bool {
	if len(args) < 2 ||
		!strings.HasPrefix(filepath.Base(args[0]), ManagedHelperName) ||
		args[1] != subcommand {
		return false
	}
	for i := 2; i+1 < len(args); i++ {
		if args[i] == "--name" {
			return args[i+1] == name
		}
	}
	return false
}

func readProcessStartTime(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("pid must be positive")
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if info.Proc.P_pid != int32(pid) {
		return "", fmt.Errorf("process %d identity unavailable", pid)
	}
	startedAt := info.Proc.P_starttime
	if startedAt.Sec <= 0 || startedAt.Usec < 0 {
		return "", fmt.Errorf("process %d start time unavailable", pid)
	}
	return fmt.Sprintf("%s%d.%06d", processIdentityPrefix, startedAt.Sec, startedAt.Usec), nil
}

func readProcessArguments(pid int) ([]string, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("pid must be positive")
	}
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return parseProcessArguments(raw)
}

func parseProcessArguments(raw []byte) ([]string, error) {
	if len(raw) < 4 {
		return nil, fmt.Errorf("process arguments are truncated")
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	if argc <= 0 || argc > 4096 {
		return nil, fmt.Errorf("invalid process argument count %d", argc)
	}
	raw = raw[4:]
	executableEnd := bytes.IndexByte(raw, 0)
	if executableEnd < 0 {
		return nil, fmt.Errorf("process executable path is unterminated")
	}
	raw = raw[executableEnd+1:]
	raw = bytes.TrimLeft(raw, "\x00")
	args := make([]string, 0, argc)
	for len(args) < argc {
		end := bytes.IndexByte(raw, 0)
		if end < 0 {
			return nil, fmt.Errorf("process argument %d is unterminated", len(args))
		}
		args = append(args, string(raw[:end]))
		raw = raw[end+1:]
	}
	return args, nil
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
	if err := ensurePrivateDir(root); err != nil {
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
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
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
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("secure metadata: %w", err)
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

func writePreparationMarker(stateRoot, name string, inst Instance) error {
	pid := os.Getpid()
	startedAt, err := readProcessStartTime(pid)
	if err != nil {
		return fmt.Errorf("read preparation process identity: %w", err)
	}
	marker := preparationMarker{
		PID:          pid,
		PIDStartedAt: startedAt,
		StartedAt:    time.Now().UTC(),
		Instance:     inst,
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("encode preparation marker: %w", err)
	}
	path := PreparationPath(stateRoot, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write preparation marker: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("commit preparation marker: %w", err)
	}
	return nil
}

func preparationActive(stateRoot, name string) bool {
	_, active := activePreparationMarker(stateRoot, name)
	return active
}

func activePreparationMarker(stateRoot, name string) (preparationMarker, bool) {
	data, err := os.ReadFile(PreparationPath(stateRoot, name))
	if err != nil {
		return preparationMarker{}, false
	}
	var marker preparationMarker
	if err := json.Unmarshal(data, &marker); err != nil || marker.PID <= 0 || marker.PIDStartedAt == "" {
		return preparationMarker{}, false
	}
	if !pidAlive(marker.PID) {
		return preparationMarker{}, false
	}
	if strings.ContainsAny(marker.PIDStartedAt, " \t\r\n") {
		matches, err := legacyProcessIdentityMatches(marker.PID, marker.PIDStartedAt, "start", name)
		if err != nil {
			return marker, true
		}
		return marker, matches
	}
	startedAt, err := processStartTime(marker.PID)
	if err != nil {
		return marker, true
	}
	return marker, startedAt == marker.PIDStartedAt
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
			if !errors.Is(err, os.ErrNotExist) {
				continue
			}
			if marker, active := activePreparationMarker(stateRoot, entry.Name()); active {
				inst = marker.Instance
				inst.Name = entry.Name()
				inst.Status = StatusStarting
				inst.Error = ""
				inst.PID = 0
				inst.PIDStartedAt = ""
				if inst.CreatedAt.IsZero() {
					inst.CreatedAt = marker.StartedAt
				}
				if inst.UpdatedAt.IsZero() {
					inst.UpdatedAt = marker.StartedAt
				}
				instances = append(instances, inst)
				continue
			}
			info, infoErr := entry.Info()
			if infoErr != nil || time.Since(info.ModTime()) < metadataLessStaleAfter {
				continue
			}
			instances = append(instances, Instance{
				Name:      entry.Name(),
				Status:    StatusStopped,
				Error:     "missing instance metadata",
				CreatedAt: info.ModTime().UTC(),
				UpdatedAt: info.ModTime().UTC(),
			})
			continue
		}
		inst = migrateLegacyProcessIdentity(inst)
		instances = append(instances, normalizeInstance(inst))
	}
	sort.Slice(instances, func(i, j int) bool { return instances[i].Name < instances[j].Name })
	return instances, nil
}

func migrateLegacyProcessIdentity(inst Instance) Instance {
	if inst.PID <= 0 || !strings.ContainsAny(inst.PIDStartedAt, " \t\r\n") {
		return inst
	}
	matches, err := processIdentityMatches(inst)
	if err != nil || !matches {
		return inst
	}
	actual, err := processStartTime(inst.PID)
	if err != nil {
		return inst
	}
	inst.PIDStartedAt = actual
	return inst
}

func normalizeInstance(inst Instance) Instance {
	if inst.SSHHost == "" && inst.SSHPort > 0 {
		inst.SSHHost = "127.0.0.1"
	}
	if inst.Status == StatusStarting && inst.PID == 0 && pidlessStartupStale(inst, time.Now().UTC()) {
		inst.Status = StatusStopped
		inst.PIDStartedAt = ""
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

func helperDaemonEnv() []string {
	env := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"LC_ALL=C",
		"LANG=C",
		"TZ=UTC",
	}
	for _, name := range []string{"HOME", "TMPDIR", ManagedHelperUseLockEnv} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func pidlessStartupStale(inst Instance, now time.Time) bool {
	updatedAt := inst.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = inst.CreatedAt
	}
	return updatedAt.IsZero() || !updatedAt.Add(pidlessStartupStaleAfter).After(now)
}

func processIdentityChanged(inst Instance) bool {
	expected := strings.TrimSpace(inst.PIDStartedAt)
	if inst.PID <= 0 || expected == "" {
		return false
	}
	matches, err := processIdentityMatches(inst)
	return err == nil && !matches
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
