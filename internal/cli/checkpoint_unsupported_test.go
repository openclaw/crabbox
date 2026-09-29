//go:build !windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func TestCheckpointCreateNativeUnsupportedReceipt(t *testing.T) {
	const leaseID = "cbx_abcdef012345"
	for _, tc := range []struct {
		name     string
		mode     string
		json     bool
		strategy string
		extra    []string
		invalid  bool
	}{
		{name: "native JSON", mode: "native", json: true},
		{name: "native human", mode: "native"},
		{name: "explicit strategy", mode: "native", strategy: "image", json: true},
		{name: "explicit auto strategy", mode: "native", strategy: "auto", json: true},
		{name: "native alias", mode: "vm", json: true},
		{name: "image alias", mode: "ami", json: true},
		{name: "snapshot alias", mode: "disk", json: true},
		{name: "before retention probe", mode: "native", json: true, extra: []string{"--expire-unused-after", "7d"}},
		{name: "reclaim", mode: "native", json: true, extra: []string{"--reclaim"}},
		{name: "invalid JSON", mode: "invalid", json: true, invalid: true},
		{name: "invalid human", mode: "invalid", invalid: true},
		{name: "invalid recipe override", mode: "invalid", json: true, invalid: true, extra: []string{"--recipe-only"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resolves, unexpected atomic.Int32
			endpoint, _ := configureManagedCheckpointTest(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/private-base/v1/leases/checkpoint-proof" {
					unexpected.Add(1)
					http.NotFound(w, r)
					return
				}
				resolves.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
					ID: leaseID, Slug: "checkpoint-proof", Provider: "hetzner", TargetOS: targetLinux,
					CloudID: "12345", Host: "127.0.0.1", SSHUser: "crabbox", SSHPort: "22", State: "active",
				}})
			})
			t.Setenv("CRABBOX_COORDINATOR", strings.Replace(endpoint.URL, "://", "://fixture-user:fixture-password@", 1)+"/private-base")
			bin := t.TempDir()
			prepared := filepath.Join(bin, "prepared-source")
			t.Setenv("CHECKPOINT_PREPARED", prepared)
			ssh := "#!/bin/sh\nfor arg do remote=$arg; done\nif [ \"$remote\" = 'exit 0' ]; then exit 0; fi\nprintf 'unexpected source preparation\\n' >> \"$CHECKPOINT_PREPARED\"\nexit 97\n"
			if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(ssh), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			var stdout, stderr bytes.Buffer
			args := []string{"--provider", "hetzner", "--id", "checkpoint-proof", "--network", "public", "--mode", tc.mode, "--wait", "--wait-timeout", "2700000ms"}
			if tc.json {
				args = append(args, "--json")
			}
			requested := "checkpoint create --mode " + tc.mode
			if tc.strategy != "" {
				args = append(args, "--strategy", tc.strategy)
				requested += " --strategy " + tc.strategy
			}
			args = append(args, tc.extra...)
			err := (App{Stdout: &stdout, Stderr: &stderr}).checkpointCreate(context.Background(), args)
			message := requested + " is unsupported for provider=hetzner target=linux through coordinator " + endpoint.URL + ": the provider does not offer native checkpoints for coordinator-brokered leases with this mode and strategy; use --mode archive or a provider configuration that offers native checkpoints"
			wantResolves := int32(1)
			if tc.invalid {
				message = "checkpoint mode must be auto, native, or archive"
				wantResolves = 0
			}
			// The executable prints this ExitError message to stderr.
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 || exitErr.Message != message {
				t.Errorf("error=%v, want exit 2: %s", err, message)
			}
			wantStdout := ""
			if tc.json && !tc.invalid {
				wantStdout = fmt.Sprintf("{\"schema\":\"crabbox.checkpoint.create.failure.v1\",\"outcome\":\"not_submitted\",\"reason\":\"native_unsupported\",\"provider\":\"hetzner\",\"leaseId\":\"%s\",\"localReservation\":\"none\",\"message\":%q}\n", leaseID, message)
			}
			if stdout.String() != wantStdout {
				t.Errorf("stdout=%q, want exactly %q", stdout.String(), wantStdout)
			}
			if stderr.Len() != 0 {
				t.Errorf("unexpected operation output on stderr: %q", stderr.String())
			}
			if resolves.Load() != wantResolves || unexpected.Load() != 0 {
				t.Errorf("lease resolutions=%d unexpected coordinator requests=%d, want %d/0", resolves.Load(), unexpected.Load(), wantResolves)
			}
			// This coordinator resolver does not claim; any claim here is create's own.
			for _, path := range []string{
				filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", leaseID+".json"),
				filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "checkpoints"),
				prepared,
			} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Errorf("unexpected side effect at %s: %v", path, err)
				}
			}
		})
	}
}

func TestCheckpointNativeUnsupportedMessage(t *testing.T) {
	for _, tc := range []struct {
		name, coordinator, strategy string
		explicit                    bool
		server                      Server
		target                      SSHTarget
		want                        string
	}{
		{
			name: "direct configuration fallback",
			want: "checkpoint create --mode native is unsupported for provider=hetzner target=linux; use --mode archive or a provider configuration that offers native checkpoints",
		},
		{
			name: "sanitized coordinator origin", coordinator: "https://fixture-user:fixture-password@COORDINATOR.EXAMPLE/private-token?token=fixture-query#fixture-fragment",
			want: "checkpoint create --mode native is unsupported for provider=hetzner target=linux through coordinator https://coordinator.example: the provider does not offer native checkpoints for coordinator-brokered leases with this mode and strategy; use --mode archive or a provider configuration that offers native checkpoints",
		},
		{
			name: "resolved provider and target with explicit strategy", strategy: "image", explicit: true,
			server: Server{Provider: "parallels"}, target: SSHTarget{TargetOS: targetMacOS},
			want: "checkpoint create --mode native --strategy image is unsupported for provider=parallels target=macos; use --mode archive or a provider configuration that offers native checkpoints",
		},
		{
			name: "explicit empty strategy", explicit: true,
			want: "checkpoint create --mode native --strategy \"\" is unsupported for provider=hetzner target=linux; use --mode archive or a provider configuration that offers native checkpoints",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Provider: "hetzner", TargetOS: targetLinux, Coordinator: tc.coordinator}
			if got := checkpointNativeUnsupportedMessage("native", tc.strategy, tc.explicit, cfg, tc.server, tc.target); got != tc.want {
				t.Errorf("message=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckpointNativeUnsupportedMessageUTF8Bound(t *testing.T) {
	for _, coordinator := range []string{"", "https://coordinator.example"} {
		for _, char := range []string{"a", "é", "界", "🦀"} {
			t.Run(coordinator+"/"+char, func(t *testing.T) {
				cfg := Config{Provider: strings.Repeat(char, 1100), TargetOS: targetLinux, Coordinator: coordinator}
				message := checkpointNativeUnsupportedMessage("native", "auto", false, cfg, Server{}, SSHTarget{})
				if len(message) > 1024 || len(message) < 1021 || !utf8.ValidString(message) {
					t.Fatalf("message length=%d valid UTF-8=%t", len(message), utf8.ValidString(message))
				}
				if !strings.Contains(message, "...") || !strings.HasSuffix(message, "; use --mode archive or a provider configuration that offers native checkpoints") {
					t.Errorf("truncated message lost its marker or next step: %q", message)
				}
			})
		}
	}
}
