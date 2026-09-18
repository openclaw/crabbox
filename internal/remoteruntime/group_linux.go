package remoteruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// The unreaped direct-child guard reserves the process-group identity until
// the supervisor has sent its final signal. No later invocation adopts it.
type commandGroup struct {
	guard    *exec.Cmd
	lifetime *os.File
	worker   *exec.Cmd
	result   chan error
	grace    time.Duration
}

func runGuard() int {
	if os.Getpid() != syscall.Getpgrp() {
		return failureCode
	}
	lifetime, ready := os.NewFile(3, "runtime-lifetime"), os.NewFile(4, "runtime-ready")
	if lifetime == nil || ready == nil {
		return failureCode
	}
	signal.Ignore(syscall.SIGTERM, syscall.SIGHUP)
	if _, err := ready.Write([]byte{1}); err != nil {
		return failureCode
	}
	_ = ready.Close()
	var one [1]byte
	for {
		if _, err := lifetime.Read(one[:]); err != nil {
			break
		}
	}
	// A lost supervisor cannot leave its workload running indefinitely.
	_ = syscall.Kill(-syscall.Getpgrp(), syscall.SIGKILL)
	return failureCode
}

func startGroup(ctx context.Context, grace time.Duration) (*commandGroup, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	lifetimeRead, lifetimeWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		_ = lifetimeRead.Close()
		_ = lifetimeWrite.Close()
		return nil, err
	}
	defer readyRead.Close()
	guard := exec.Command(executable, Command, "guard", Protocol)
	guard.ExtraFiles = []*os.File{lifetimeRead, readyWrite}
	guard.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = guard.Start()
	_ = lifetimeRead.Close()
	_ = readyWrite.Close()
	if err != nil {
		_ = lifetimeWrite.Close()
		return nil, err
	}
	g := &commandGroup{guard: guard, lifetime: lifetimeWrite, grace: grace}
	ready := make(chan error, 1)
	go func() {
		var value [1]byte
		_, err := io.ReadFull(readyRead, value[:])
		if err == nil && value[0] != 1 {
			err = errors.New("invalid runtime guard acknowledgement")
		}
		ready <- err
	}()
	select {
	case err = <-ready:
	case <-ctx.Done():
		err = context.Cause(ctx)
	}
	if err != nil {
		if cleanupErr := g.close(); cleanupErr != nil {
			// A non-nil group on failure retains unconfirmed cleanup evidence.
			return g, errors.Join(err, cleanupErr)
		}
		return nil, err
	}
	return g, nil
}

func (g *commandGroup) start(command string, input, stdout, stderr *os.File) error {
	g.worker = exec.Command("/bin/sh", command)
	g.worker.Stdin, g.worker.Stdout, g.worker.Stderr = input, stdout, stderr
	g.worker.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: g.guard.Process.Pid}
	if err := g.worker.Start(); err != nil {
		return err
	}
	g.result = make(chan error, 1)
	go func() { g.result <- g.worker.Wait() }()
	return nil
}

func (g *commandGroup) close() error {
	group := g.guard.Process.Pid
	termErr := syscall.Kill(-group, syscall.SIGTERM)
	// Preserve the TERM grace separately from the post-KILL observation budget.
	time.Sleep(g.grace)
	killErr := syscall.Kill(-group, syscall.SIGKILL)
	_ = g.lifetime.Close()
	deadline := time.NewTimer(g.grace)
	defer deadline.Stop()
	guardDone := make(chan error, 1)
	go func() { guardDone <- g.guard.Wait() }()
	select {
	case err := <-guardDone:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() {
			return errors.New("runtime guard termination unconfirmed")
		}
	case <-deadline.C:
		return errors.New("runtime guard reaping unconfirmed")
	}
	if g.result != nil {
		select {
		case <-g.result:
		case <-deadline.C:
			return errors.New("runtime worker reaping unconfirmed")
		}
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := syscall.Kill(-group, 0)
		if errors.Is(err, syscall.ESRCH) {
			return errors.Join(termErr, killErr)
		}
		if err != nil {
			return fmt.Errorf("runtime group absence unconfirmed: %w", err)
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return errors.New("runtime group absence unconfirmed")
		}
	}
}
