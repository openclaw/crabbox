package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/openclaw/crabbox/internal/remoteruntime"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] != remoteruntime.Command {
		fmt.Fprintln(os.Stderr, "invalid remote runtime invocation")
		return 74
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	return remoteruntime.RunCLI(ctx, args[1:], os.Stdin, os.Stdout, os.Stderr)
}
