package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/openclaw/crabbox/internal/remoteruntime"
	"github.com/openclaw/crabbox/internal/runner"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	return runWithIO(ctx, args, os.Stdin, os.Stdout, os.Stderr)
}

func runWithIO(ctx context.Context, args []string, stdin, stdout, stderr *os.File) int {
	if len(args) != 0 {
		switch args[0] {
		case remoteruntime.Command:
			return remoteruntime.RunCLI(ctx, args[1:], stdin, stdout, stderr)
		case runner.Command:
			return runner.Main(ctx, args[1:], stdin, stdout, stderr)
		}
	}
	fmt.Fprintln(stderr, "invalid remote runtime invocation")
	return 74
}
