package main

import (
	"context"
	"fmt"
	"io"
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
		printError(os.Stderr, err)
		var exit cli.ExitError
		if cli.AsExitError(err, &exit) {
			os.Exit(exit.Code)
		}
		os.Exit(1)
	}
}

func printError(w io.Writer, err error) {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			printError(w, cause)
		}
		return
	}
	var exit cli.ExitError
	if cli.AsExitError(err, &exit) {
		// Keep standalone exit diagnostics unchanged, but visit joins inside
		// wrappers so their independent causes are not hidden by errors.As.
		if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			printError(w, wrapped.Unwrap())
		} else if exit.Message != "" {
			fmt.Fprintln(w, exit.Message)
		}
		return
	}
	if err != nil {
		fmt.Fprintln(w, err)
	}
}
