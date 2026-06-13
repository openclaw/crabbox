package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/openclaw/crabbox/internal/cli"
	_ "github.com/openclaw/crabbox/internal/providers/all"
	"github.com/openclaw/crabbox/internal/providers/vercelsandbox"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	args := os.Args[1:]
	var err error
	if len(args) > 0 && args[0] == "__vercel-sandbox-bridge" {
		err = vercelsandbox.RunBridgeCLI(ctx, os.Stdin, os.Stdout, os.Stderr)
	} else {
		err = cli.Run(ctx, args)
	}
	if err != nil {
		var exit cli.ExitError
		if cli.AsExitError(err, &exit) {
			if exit.Message != "" {
				fmt.Fprintln(os.Stderr, exit.Message)
			}
			os.Exit(exit.Code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
