package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errLocalCommandReservation = errors.New("local command leader reservation contradicted")
var errLocalCommandDescendants = errors.New("local command exited with live process-group members")

// ValidateLocalCommandProcessGroupJoin permits only a standalone group whose
// owner can terminate and join it without killing an enclosing controller.
func ValidateLocalCommandProcessGroupJoin(ctx context.Context) error {
	_, err := prepareLocalCommandGroupInspection(ctx)
	return err
}

func prepareLocalCommandGroupInspection(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", errors.Join(err, context.Cause(ctx))
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return "", fmt.Errorf("joined local command groups require macOS or Linux")
	}
	if os.Getenv(controllerProcessTreeOwnedEnv) == "1" {
		return "", fmt.Errorf("joined local command groups cannot run inside a controller-owned process group")
	}
	command := systemInspectionCommand("ps", "-axo", "pgid=,stat=")
	if command.Err != nil {
		return "", fmt.Errorf("joined local command groups require compatible ps on PATH (install procps on Linux): %w", command.Err)
	}
	path, err := filepath.Abs(command.Path)
	if err != nil {
		return "", err
	}
	groups, err := inspectLocalCommandGroups(ctx, path)
	if ctx.Err() != nil {
		return "", errors.Join(err, ctx.Err(), context.Cause(ctx))
	}
	if err == nil && !groups[localCommandProcessGroup()] {
		err = errors.New("ps did not report the current live process group")
	}
	if err != nil {
		return "", fmt.Errorf("joined local command groups require ps supporting -axo pgid=,stat= (install procps on Linux): %w", err)
	}
	return path, nil
}

func inspectLocalCommandGroups(ctx context.Context, path string) (map[int]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := systemInspectionCommand(path, "-axo", "pgid=,stat=")
	result, err := (execCommandRunner{}).Run(ctx, LocalCommandRequest{
		Name: command.Path, Args: command.Args[1:], Env: command.Env,
		MaxCapturedOutputBytes: 1024 * 1024,
	})
	if err != nil {
		return nil, err
	}
	groups := make(map[int]bool)
	for _, line := range strings.Split(result.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, errors.New("incompatible ps process-group output")
		}
		group, err := strconv.Atoi(fields[0])
		state := fields[1]
		if err != nil || group < 0 || !strings.ContainsRune("RSDZTWIPUKXtx?", rune(state[0])) {
			return nil, errors.New("incompatible ps process-group output")
		}
		groups[group] = groups[group] || state[0] != 'Z'
	}
	if len(groups) == 0 {
		return nil, errors.New("empty ps process-group output")
	}
	return groups, nil
}

type localCommandGroupOwner struct {
	cmd            *exec.Cmd
	ps             string
	grace          time.Duration
	onPending      func(error)
	once           sync.Once
	err            error
	waitReady      chan struct{}
	pending        sync.Once
	mu             sync.Mutex
	reservationErr error
}

func configureJoinedLocalCommand(ctx context.Context, cmd *exec.Cmd, grace time.Duration, onPending func(error)) (*localCommandGroupOwner, error) {
	ps, err := prepareLocalCommandGroupInspection(ctx)
	if err != nil {
		return nil, err
	}
	if grace <= 0 {
		grace = controllerChildWaitDelay
	}
	owner := &localCommandGroupOwner{cmd: cmd, ps: ps, grace: grace, onPending: onPending, waitReady: make(chan struct{})}
	configureDaemonCommand(cmd)
	cmd.Cancel = owner.cancel
	return owner, nil
}

func (o *localCommandGroupOwner) groupAlive(group int) bool {
	// Cleanup outlives caller cancellation, but each observation remains bounded.
	// Loss of the selected observer cannot prove the reserved group has closed.
	groups, err := inspectLocalCommandGroups(context.Background(), o.ps)
	if err != nil {
		o.emitPending(fmt.Errorf("local command failed; process-group cleanup pending: restore compatible ps at %s: %w", o.ps, err))
		return true
	}
	return groups[group]
}

func (o *localCommandGroupOwner) cancel() error {
	err := o.join(false)
	// Do not let os/exec arm its fallback kill while the main owner might
	// still reject the reservation and refuse to reap this integer PID.
	<-o.waitReady
	return err
}

func (o *localCommandGroupOwner) beforeWait() error {
	for {
		exited, err := observeLocalCommandLeader(o.cmd.Process.Pid, os.Getpid())
		if errors.Is(err, errLocalCommandReservation) {
			o.holdContradictedReservation(err)
		}
		if err != nil || exited {
			joinErr := o.join(exited)
			if err != nil {
				return errors.Join(err, joinErr)
			}
			return joinErr
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (o *localCommandGroupOwner) holdContradictedReservation(err error) {
	o.mu.Lock()
	if o.reservationErr == nil {
		o.reservationErr = fmt.Errorf("local command failed; cleanup pending without signal or reap: %w", err)
	}
	failure := o.reservationErr
	o.mu.Unlock()
	// A contradicted reservation cannot be reacquired by inspecting the same
	// integer again. Keep this caller and its claim; only forced owner exit
	// can end this fail-closed branch.
	defer func() {
		for {
			time.Sleep(time.Second)
		}
	}()
	o.emitPending(failure)
}

func (o *localCommandGroupOwner) emitPending(err error) {
	o.pending.Do(func() {
		if o.onPending != nil {
			o.onPending(err)
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
	})
}

func (o *localCommandGroupOwner) join(afterExit bool) error {
	if o.cmd.Process == nil {
		return os.ErrProcessDone
	}
	o.mu.Lock()
	reservationErr := o.reservationErr
	o.mu.Unlock()
	if reservationErr != nil {
		o.holdContradictedReservation(reservationErr)
	}
	o.once.Do(func() {
		deadline := time.Now().Add(o.grace)
		group := o.cmd.Process.Pid
		stop := func(group int) error {
			o.mu.Lock()
			if o.reservationErr != nil {
				failure := o.reservationErr
				o.mu.Unlock()
				o.holdContradictedReservation(failure)
			}
			defer o.mu.Unlock()
			return stopControllerProcessGroup(group)
		}
		o.err = joinLocalCommandProcessGroup(group, stop, o.groupAlive, deadline, o.emitPending, afterExit)
	})
	return o.err
}

func joinLocalCommandProcessGroup(group int, stop func(int) error, alive func(int) bool, deadline time.Time, onPending func(error), afterExit bool) error {
	if !alive(group) {
		return nil
	}
	var exitErr error
	if afterExit {
		exitErr = errLocalCommandDescendants
	}
	stopErr := stop(group)
	if err := waitForControllerProcessGroupExit(group, stopErr, alive, deadline); err != nil {
		pending := fmt.Errorf("local command failed; process-group cleanup pending: %w", err)
		// Even a failing notification cannot release the caller's claim while
		// live group members remain. This continuation belongs to the same Run.
		defer func() {
			for alive(group) {
				time.Sleep(10 * time.Millisecond)
			}
		}()
		if onPending != nil {
			onPending(pending)
		} else {
			fmt.Fprintln(os.Stderr, pending)
		}
		return errors.Join(exitErr, pending)
	}
	return exitErr
}
