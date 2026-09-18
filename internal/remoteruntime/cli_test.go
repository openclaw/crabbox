package remoteruntime

import (
	"context"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == Command {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		os.Exit(RunCLI(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func TestRequest(t *testing.T) {
	args := []string{"run", Protocol, strings.Repeat("a", 32), "12", "34", "15000", "5000", "90000", "preflight", "watched"}
	got, err := parseRequest(args)
	want := request{nonce: args[2], commandBytes: 12, inputBytes: 34, idle: 15 * time.Second, grace: 5 * time.Second, execution: 90 * time.Second, preflight: true, watchInput: true}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("request = %+v, %v", got, err)
	}
	for _, tc := range []struct {
		name  string
		index int
		value string
	}{
		{"protocol", 1, "CBX-REMOTE-0"},
		{"nonce", 2, "short"},
		{"command limit", 3, "67108865"},
		{"frame limit", 4, "1099511627776"},
		{"zero idle", 5, "0"},
		{"zero grace", 6, "0"},
		{"duration overflow", 7, "9223372036854775807"},
		{"signed duration", 7, "-1"},
		{"mode", 8, "unknown"},
		{"input mode", 9, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := append([]string(nil), args...)
			changed[tc.index] = tc.value
			if _, err := parseRequest(changed); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
}

func TestControlRequest(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	for _, action := range []string{"observe", "cancel", "retire"} {
		got, err := parseControlRequest([]string{"control", Protocol, nonce, action})
		if err != nil || got.nonce != nonce || got.action != action || got.timeout != 15*time.Second {
			t.Fatalf("control %s = %+v, %v", action, got, err)
		}
	}
	for _, allowance := range []string{"1", "2371", "10000"} {
		got, err := parseControlRequest([]string{"control", Protocol, nonce, "cleanup", allowance})
		if err != nil || got.timeout <= 0 || got.timeout > MaxCleanupWait {
			t.Fatalf("cleanup allowance %s: %+v %v", allowance, got, err)
		}
	}
	for _, args := range [][]string{
		{"control", Protocol, nonce, "cleanup"},
		{"control", Protocol, nonce, "cleanup", "0"},
		{"control", Protocol, nonce, "cleanup", "-1"},
		{"control", Protocol, nonce, "cleanup", "10001"},
		{"control", Protocol, nonce, "cleanup", "1ms"},
		{"control", Protocol, nonce, "retire", "10000"},
	} {
		if _, err := parseControlRequest(args); err == nil {
			t.Fatalf("invalid control accepted: %v", args)
		}
	}
}
