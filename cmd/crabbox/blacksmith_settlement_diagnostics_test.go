package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"text/tabwriter"

	"github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/blacksmith"
	"github.com/openclaw/crabbox/internal/testutil"
)

type settlementDiagnosticRunner func(context.Context, cli.LocalCommandRequest) (cli.LocalCommandResult, error)

func (f settlementDiagnosticRunner) Run(ctx context.Context, req cli.LocalCommandRequest) (cli.LocalCommandResult, error) {
	return f(ctx, req)
}

func TestPrintBlacksmithSettlementDiagnostics(t *testing.T) {
	for _, method := range []string{"GET", "POST", "status", "stop"} {
		t.Run(method, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			t.Setenv("BLACKSMITH_API_URL", "")
			t.Setenv("BLACKSMITH_ORG", "")
			const id = "tbx_cli_diagnostic"
			const runURL = "https://github.com/example-org/my-app/actions/runs/123456789"
			claim := cli.LeaseClaim{LeaseID: id, CloudID: id, Slug: "diagnostic-krill", Provider: "blacksmith-testbox", RepoRoot: t.TempDir(), ProviderScope: `{"api":"https://backend.blacksmith.sh","org":"example-org"}`, Labels: map[string]string{"provider": "blacksmith-testbox", "lease": id, "slug": "diagnostic-krill", "workflow": ".github/workflows/testbox.yml", "job": "go", "ref": "main"}}
			if err := cli.WithDurableLeaseClaimLock(id, func(current *cli.LeaseClaim, exists bool, save func() error) error {
				if exists {
					t.Fatal("fixture claim already exists")
				}
				*current = claim
				if err := save(); err != nil {
					return err
				}
				claim = *current
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			key, err := cli.TestboxKeyPath(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(key, []byte("synthetic key marker"), 0o600); err != nil {
				t.Fatal(err)
			}
			var status strings.Builder
			w := tabwriter.NewWriter(&status, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tSTATUS\tIP\tWORKFLOW\tJOB\tREF\tCREATED\tRUN URL")
			fmt.Fprintf(w, "%s\tcompleted\t\t.github/workflows/testbox.yml\tgo\tmain\t2026-09-02T13:54:37.000000Z\t%s\n", id, runURL)
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			private := cli.Exit(17, "synthetic-private-provider-credential")
			const nativeOutput = "synthetic-private-native-output"
			failedCommands := 0
			cfg := cli.BaseConfig()
			cfg.Blacksmith.Org = "example-org"
			var stdout, stderr bytes.Buffer
			backend, err := (blacksmith.Provider{}).Configure(cfg, cli.Runtime{
				Stdout: &stdout, Stderr: &stderr,
				Exec: settlementDiagnosticRunner(func(_ context.Context, req cli.LocalCommandRequest) (cli.LocalCommandResult, error) {
					if req.Name == "blacksmith" {
						if strings.Contains(strings.Join(req.Args, " "), "testbox "+method) {
							failedCommands++
							if method == "stop" {
								cancel()
							}
							return cli.LocalCommandResult{ExitCode: 1, Stderr: nativeOutput}, private
						}
						if strings.Contains(strings.Join(req.Args, " "), "testbox status") {
							return cli.LocalCommandResult{Stdout: status.String()}, nil
						}
						return cli.LocalCommandResult{}, nil
					}
					if req.Name != "gh" || len(req.Args) < 5 {
						t.Fatalf("unexpected command: %+v", req)
					}
					if req.Args[4] != method {
						return cli.LocalCommandResult{Stdout: fmt.Sprintf(`{"id":123456789,"html_url":%q,"status":"in_progress"}`, runURL)}, nil
					}
					failedCommands++
					if method == "POST" {
						cancel()
					}
					return cli.LocalCommandResult{ExitCode: 1, Stderr: "synthetic-private-api-output"}, private
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			failure := backend.(cli.DelegatedRunBackend).Stop(ctx, cli.StopRequest{ID: id})
			if !errors.Is(failure, private) || failedCommands != 1 {
				t.Fatalf("typed cause or command count lost: %v, calls=%d", failure, failedCommands)
			}
			// Unresolved stops intentionally forward native output. Only prepared
			// diagnostics hide private causes; they do not redact provider streams.
			wantStderr := ""
			if method == "stop" {
				wantStderr = nativeOutput
			}
			if stdout.Len() != 0 || stderr.String() != wantStderr {
				t.Errorf("native console contract changed: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			wantCode := 5
			wantDiagnostic := "gh exit=1 cause=other"
			if method != "GET" {
				wantCode = 1
			}
			if method == "status" || method == "stop" {
				wantDiagnostic = "blacksmith testbox " + method + " exit=1 cause=other"
			}
			for _, wrapped := range []error{failure, fmt.Errorf("wrapper: %w", failure), errors.Join(failure, errors.New("visible sibling cleanup failure"))} {
				var output bytes.Buffer
				printError(&output, wrapped)
				if strings.Contains(output.String(), "synthetic-private") ||
					!strings.Contains(output.String(), wantDiagnostic) {
					t.Errorf("unsafe or missing CLI diagnostic: %q", output.String())
				}
				console := stderr.String() + output.String()
				if strings.Contains(console, private.Error()) || !strings.Contains(console, "phase=reconciliation") {
					t.Errorf("combined CLI exposed a private cause or lost cleanup phase: %q", console)
				}
				if (method == "GET" || method == "POST") && !strings.Contains(output.String(), "exact GitHub run") {
					t.Error("GitHub operation context lost")
				}
				if wrapped != failure && strings.Contains(wrapped.Error(), "visible sibling") && !strings.Contains(output.String(), "visible sibling cleanup failure") {
					t.Error("display boundary swallowed a sibling failure")
				}
				var selected cli.ExitError
				if !cli.AsExitError(wrapped, &selected) || selected.Code != wantCode || !errors.Is(wrapped, private) {
					t.Error("display boundary changed the selected exit")
				}
			}
			if got, present, err := cli.ReadLeaseClaimWithPresence(id); err != nil || !present || !reflect.DeepEqual(got, claim) {
				t.Fatalf("failed settlement changed custody: %v", err)
			}
			if got, err := os.ReadFile(key); err != nil || string(got) != "synthetic key marker" {
				t.Fatalf("failed settlement changed the owned key: %v", err)
			}
		})
	}
}
