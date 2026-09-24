package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
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
	var printed []string
	printMessage := func(message string) {
		for _, previous := range printed {
			if strings.Contains(previous, message) {
				return
			}
		}
		fmt.Fprintln(w, message)
		printed = append(printed, message)
	}
	var exit cli.ExitError
	if cli.AsExitError(err, &exit) && exit.Message != "" {
		// Providers may already include their causes in the exit diagnostic.
		// Print it first even when a joined cause precedes it in the error tree.
		printMessage(exit.Message)
	}
	var printCauses func(error)
	printCauses = func(err error) {
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			causes := joined.Unwrap()
			var exit cli.ExitError
			if !cli.AsExitError(err, &exit) {
				// Multi-%w formatters carry context that a plain newline join does not.
				flat := errors.Join(causes...)
				if flat == nil || err.Error() != flat.Error() {
					printMessage(err.Error())
					return
				}
			}
			for _, cause := range causes {
				printCauses(cause)
			}
			return
		}
		var exit cli.ExitError
		if cli.AsExitError(err, &exit) {
			// Preserve standalone exit output while visiting joins inside wrappers.
			if wrapped, ok := err.(interface{ Unwrap() error }); ok {
				printCauses(wrapped.Unwrap())
			} else if exit.Message != "" {
				printMessage(exit.Message)
			}
			return
		}
		if err != nil {
			printMessage(err.Error())
		}
	}
	printCauses(err)
}
