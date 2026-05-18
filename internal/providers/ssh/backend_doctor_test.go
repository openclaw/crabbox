package ssh

import (
	"context"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStaticSSHDoctorDoesNotReportProbeWhenUnchecked(t *testing.T) {
	cfg := Config{}
	cfg.Static.Host = "example.test"
	backend := NewStaticSSHLeaseBackend(Provider{}.Spec(), cfg, Runtime{}).(*staticLeaseBackend)

	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Message, "api=static_config") || strings.Contains(result.Message, "api=ssh_probe") {
		t.Fatalf("result=%#v", result)
	}
	if !strings.Contains(result.Message, "runtime=unchecked") {
		t.Fatalf("result=%#v", result)
	}
}
