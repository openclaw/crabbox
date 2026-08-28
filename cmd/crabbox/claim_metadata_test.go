//go:build !windows

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

type claimMetadataOutput struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	marker  string
	reached chan struct{}
}

func (w *claimMetadataOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buffer.Write(p)
	if strings.Contains(w.buffer.String(), w.marker) {
		select {
		case w.reached <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (w *claimMetadataOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func TestClaimMetadataBlacksmithCLIUnderUnrelatedOwner(t *testing.T) {
	for _, mode := range []string{"claimed ID", "unclaimed ID", "slug", "warmup", "other repo"} {
		t.Run(mode, func(t *testing.T) {
			dirs := testutil.IsolateUserDirs(t)
			repo := t.TempDir()
			t.Chdir(repo)
			config := filepath.Join(repo, "crabbox.yaml")
			if err := os.WriteFile(config, []byte("provider: blacksmith-testbox\nblacksmith:\n  workflow: .github/workflows/testbox.yml\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", config)
			t.Setenv("CRABBOX_PROVIDER", "")
			t.Setenv("CRABBOX_COORDINATOR", "")
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
			t.Setenv("CRABBOX_ENV_ALLOW", "")
			bin := t.TempDir()
			callsPath := filepath.Join(bin, "calls")
			t.Setenv("CRABBOX_TEST_CALLS", callsPath)
			fake := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$CRABBOX_TEST_CALLS"
case "$1 $2" in
  'testbox warmup') printf 'ready tbx_metadata123\n' ;;
  'testbox run') printf 'Sync complete\nfixture-executed\n' ;;
  *) exit 91 ;;
esac
`
			if err := os.WriteFile(filepath.Join(bin, "blacksmith"), []byte(fake), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			const id = "tbx_metadata123"
			if mode == "claimed ID" || mode == "slug" || mode == "other repo" {
				claimRepo := repo
				if mode == "other repo" {
					claimRepo = filepath.Join(dirs.Root, "other-repo")
				}
				if err := cli.ClaimLeaseForRepoProvider(id, "metadata-sibling", "blacksmith-testbox", claimRepo, time.Minute, false); err != nil {
					t.Fatal(err)
				}
			}
			const ownerID = "cbx_metadata_owner"
			if err := cli.ClaimLeaseForRepoProvider(ownerID, "metadata-owner", "aws", "/repo/owner", time.Minute, false); err != nil {
				t.Fatal(err)
			}
			owner, err := cli.ReadLeaseClaim(ownerID)
			if err != nil {
				t.Fatal(err)
			}
			entered, proceed := make(chan struct{}), make(chan struct{})
			ownerDone := make(chan error, 1)
			go func() {
				ownerDone <- cli.WithLeaseClaimUnchanged(ownerID, owner, func() error {
					close(entered)
					<-proceed
					return nil
				})
			}()
			var once sync.Once
			release := func() { once.Do(func() { close(proceed) }) }
			defer func() {
				release()
				if err := <-ownerDone; err != nil {
					t.Error(err)
				}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("owner did not enter")
			}
			args := []string{"run", "--id", id, "--", "true"}
			if mode == "slug" {
				args[2] = "metadata-sibling"
			} else if mode == "warmup" {
				args = []string{"warmup", "--keep"}
			}
			var stderr bytes.Buffer
			stdout := claimMetadataOutput{marker: "fixture-executed", reached: make(chan struct{}, 1)}
			if mode == "warmup" {
				stdout.marker = "warmup complete"
			}
			done := make(chan error, 1)
			go func() { done <- (cli.App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), args) }()
			completed := false
			select {
			case err = <-done:
				completed = true
			case <-stdout.reached:
				// The real adapter reached fake execution while the owner is held.
			case <-time.After(5 * time.Second):
				release()
				err = <-done
				t.Fatalf("CLI did not reach fake execution/warmup completion under live owner; after release err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			if !completed {
				select {
				case err = <-done:
				case <-time.After(5 * time.Second):
					release()
					err = <-done
					t.Fatalf("CLI did not finish after execution handshake: %v", err)
				}
			}
			calls, _ := os.ReadFile(callsPath)
			if mode == "other repo" {
				if err == nil || !strings.Contains(err.Error(), "claimed by repo") || len(calls) != 0 {
					t.Fatalf("repo guard lost: err=%v calls=%q", err, calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("CLI err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			if mode == "warmup" {
				if !strings.Contains(stdout.String(), "warmup complete") {
					t.Fatalf("warmup did not finish: %q", stdout.String())
				}
			} else if !strings.Contains(string(calls), "testbox run --id "+id) || !strings.Contains(stdout.String(), "fixture-executed") {
				t.Fatalf("exact ID did not execute: calls=%q stdout=%q", calls, stdout.String())
			}
		})
	}
}
