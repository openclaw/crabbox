package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	herdrPluginContextEnv          = "HERDR_PLUGIN_CONTEXT_JSON"
	herdrPluginRefreshIntervalEnv  = "CRABBOX_HERDR_REFRESH_INTERVAL"
	herdrPluginDefaultRefreshDelay = 3 * time.Second
)

var errHerdrPluginSelectionCancelled = errors.New("selection cancelled")

// herdrInvocationContext contains the workspace paths in Herdr's v0.7 plugin
// invocation contract. Keep this deliberately narrow so unsupported context
// shapes fail clearly instead of becoming an accidental compatibility surface.
type herdrInvocationContext struct {
	FocusedPaneCWD string `json:"focused_pane_cwd"`
	WorkspaceCWD   string `json:"workspace_cwd"`
}

func (a App) herdrPlugin(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return exit(2, "usage: crabbox __herdr-plugin <boxes|connect|doctor|job|prewarm|warmup|context-cwd>")
	}
	switch args[0] {
	case "boxes":
		return a.herdrPluginBoxes(ctx, args[1:])
	case "connect":
		return a.herdrPluginConnect(ctx, args[1:])
	case "doctor":
		return a.herdrPluginInWorkspace(ctx, args[1:], a.doctor)
	case "job":
		return a.herdrPluginJob(ctx, args[1:])
	case "prewarm":
		return a.herdrPluginInWorkspace(ctx, args[1:], a.prewarm)
	case "warmup":
		return a.herdrPluginInWorkspace(ctx, args[1:], a.warmup)
	case "context-cwd":
		if len(args) != 1 {
			return exit(2, "usage: crabbox __herdr-plugin context-cwd")
		}
		cwd, err := herdrPluginContextCWD(os.Getenv(herdrPluginContextEnv))
		if err != nil {
			return err
		}
		fmt.Fprintln(a.Stdout, cwd)
		return nil
	default:
		return exit(2, "unknown Herdr plugin command %q", args[0])
	}
}

func (a App) herdrPluginInWorkspace(ctx context.Context, args []string, run func(context.Context, []string) error) error {
	if err := chdirHerdrPluginWorkspace(); err != nil {
		return err
	}
	return run(ctx, args)
}

func chdirHerdrPluginWorkspace() error {
	cwd, err := herdrPluginContextCWD(os.Getenv(herdrPluginContextEnv))
	if err != nil {
		return err
	}
	if err := os.Chdir(cwd); err != nil {
		return fmt.Errorf("open Herdr workspace %q: %w", cwd, err)
	}
	return nil
}

func herdrPluginContextCWD(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", exit(2, "%s is missing; invoke this command through the Crabbox Herdr plugin", herdrPluginContextEnv)
	}
	var invocation herdrInvocationContext
	if err := json.Unmarshal([]byte(raw), &invocation); err != nil {
		return "", exit(2, "parse %s: %v", herdrPluginContextEnv, err)
	}
	for _, candidate := range []string{
		invocation.FocusedPaneCWD,
		invocation.WorkspaceCWD,
	} {
		if strings.TrimSpace(candidate) != "" {
			return candidate, nil
		}
	}
	return "", exit(2, "Herdr plugin context has no focused pane or workspace cwd")
}

func (a App) herdrPluginBoxes(ctx context.Context, args []string) error {
	fs := newFlagSet("__herdr-plugin boxes", a.Stderr)
	once := fs.Bool("once", false, "render once instead of refreshing")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return exit(2, "usage: crabbox __herdr-plugin boxes [--once]")
	}
	if err := chdirHerdrPluginWorkspace(); err != nil {
		return err
	}
	refreshDelay, err := herdrPluginRefreshDelay(os.Getenv(herdrPluginRefreshIntervalEnv))
	if err != nil {
		return err
	}
	for {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		listApp := a
		listApp.Stdout = &stdout
		listApp.Stderr = &stderr
		listErr := listApp.list(ctx, nil)

		if !*once {
			fmt.Fprint(a.Stdout, "\x1b[2J\x1b[H")
		}
		fmt.Fprintln(a.Stdout, "Crabbox boxes")
		fmt.Fprintf(a.Stdout, "workspace: %s\n", mustCurrentWorkingDirectory())
		if !*once {
			fmt.Fprintf(a.Stdout, "refresh: %s  close: Ctrl+C\n", refreshDelay)
		}
		fmt.Fprintln(a.Stdout)
		if output := strings.TrimSpace(stdout.String()); output != "" {
			fmt.Fprintln(a.Stdout, output)
		} else if listErr == nil {
			fmt.Fprintln(a.Stdout, "No leases found for the configured provider.")
			fmt.Fprintln(a.Stdout, "Use the Crabbox warmup or prewarm action to create one.")
		}
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			fmt.Fprintln(a.Stdout)
			fmt.Fprintln(a.Stdout, detail)
		}
		if listErr != nil {
			fmt.Fprintln(a.Stdout)
			fmt.Fprintf(a.Stdout, "list failed: %v\n", listErr)
			if *once {
				return listErr
			}
		}
		if *once {
			return nil
		}

		timer := time.NewTimer(refreshDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func herdrPluginRefreshDelay(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return herdrPluginDefaultRefreshDelay, nil
	}
	delay, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || delay < time.Second {
		return 0, exit(2, "%s must be a duration of at least 1s", herdrPluginRefreshIntervalEnv)
	}
	return delay, nil
}

func mustCurrentWorkingDirectory() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "-"
	}
	return cwd
}

func (a App) herdrPluginConnect(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return exit(2, "usage: crabbox __herdr-plugin connect")
	}
	if err := chdirHerdrPluginWorkspace(); err != nil {
		return err
	}
	lines, err := a.herdrPluginCaptureLines(ctx, "leases", func(app App) error {
		return app.list(ctx, nil)
	})
	if err != nil {
		return err
	}
	selected, err := herdrPluginPick(a.input(), a.Stdout, "lease", lines)
	if errors.Is(err, errHerdrPluginSelectionCancelled) {
		return nil
	}
	if err != nil {
		return err
	}
	leaseID := herdrPluginLeaseID(selected)
	if leaseID == "" {
		return exit(2, "could not read a lease id from %q", selected)
	}
	return a.connect(ctx, []string{leaseID})
}

func (a App) herdrPluginJob(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return exit(2, "usage: crabbox __herdr-plugin job")
	}
	if err := chdirHerdrPluginWorkspace(); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	names := configuredJobNames(cfg)
	if len(names) == 0 {
		return exit(2, "no Crabbox jobs are configured in this workspace")
	}
	selected, err := herdrPluginPick(a.input(), a.Stdout, "job", names)
	if errors.Is(err, errHerdrPluginSelectionCancelled) {
		return nil
	}
	if err != nil {
		return err
	}
	return a.jobRun(ctx, []string{"--", selected})
}

func (a App) herdrPluginCaptureLines(ctx context.Context, label string, run func(App) error) ([]string, error) {
	var stdout bytes.Buffer
	captureApp := a
	captureApp.Stdout = &stdout
	if err := run(captureApp); err != nil {
		return nil, err
	}
	lines := nonBlankLines(stdout.String())
	if len(lines) == 0 {
		return nil, exit(2, "no Crabbox %s found for the configured workspace and provider", label)
	}
	return lines, nil
}

func nonBlankLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func herdrPluginPick(input io.Reader, output io.Writer, label string, choices []string) (string, error) {
	if len(choices) == 0 {
		return "", exit(2, "no %s choices available", label)
	}
	fmt.Fprintf(output, "Crabbox %ss:\n", label)
	for index, choice := range choices {
		fmt.Fprintf(output, "  %d) %s\n", index+1, choice)
	}
	scanner := bufio.NewScanner(input)
	for {
		fmt.Fprintf(output, "Select %s [1-%d, q to cancel]: ", label, len(choices))
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", fmt.Errorf("read %s selection: %w", label, err)
			}
			return "", exit(2, "%s selection input closed", label)
		}
		value := strings.TrimSpace(scanner.Text())
		if value == "q" || value == "quit" {
			return "", errHerdrPluginSelectionCancelled
		}
		selection, err := strconv.Atoi(value)
		if err == nil && selection >= 1 && selection <= len(choices) {
			return choices[selection-1], nil
		}
		fmt.Fprintf(output, "Enter a number from 1 to %d, or q.\n", len(choices))
	}
}

func herdrPluginLeaseID(line string) string {
	fields := strings.Fields(line)
	for _, prefix := range []string{"lease=", "slug="} {
		for _, field := range fields {
			if value, ok := strings.CutPrefix(field, prefix); ok && value != "" && value != "-" {
				return value
			}
		}
	}
	if len(fields) > 0 {
		return fields[0]
	}
	return ""
}
