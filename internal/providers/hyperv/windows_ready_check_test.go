package hyperv

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf16"

	core "github.com/openclaw/crabbox/internal/cli"
)

func decodePowerShellCommand(t *testing.T, command string) string {
	t.Helper()
	_, payload, ok := strings.Cut(command, " -EncodedCommand ")
	if !ok {
		t.Fatalf("not an encoded PowerShell command: %q", command)
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	return string(utf16.Decode(units))
}

func TestPrepareLeaseSetsBootstrapMatchedReadyChecks(t *testing.T) {
	for _, tc := range []struct {
		target    string
		wantReady bool
	}{{core.TargetWindows, true}, {core.TargetLinux, true}} {
		b := testBackend(&recordingRunner{})
		cfg := b.cfg
		cfg.TargetOS = tc.target
		cfg.WindowsMode = core.WindowsModeNormal
		lease, err := b.prepareLease(t.Context(), cfg, hypervVM{Name: "crabbox-ready-check-test"}, "192.0.2.10", core.LeaseClaim{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if (lease.SSH.ReadyCheck != "") != tc.wantReady {
			t.Fatalf("target %s ready check=%q", tc.target, lease.SSH.ReadyCheck)
		}
		if tc.target == core.TargetWindows && strings.Contains(decodePowerShellCommand(t, lease.SSH.ReadyCheck), "node") {
			t.Fatal("windows lease ready check must not require node")
		}
	}
}
