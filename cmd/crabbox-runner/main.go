package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/openclaw/crabbox/internal/runner"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runner.Main(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
