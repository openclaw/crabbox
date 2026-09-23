package cli

import (
	"testing"
)

func TestEgressClientSessionProcessMatchesExactIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"detached client", []string{egressRemoteBinary, "egress", "client", egressTicketChildArg, "--coordinator", "https://broker.example", "--id", "lease", "--session", "old", "--listen", "127.0.0.1:3128"}, true},
		{"bootstrap client", []string{egressRemoteBinary, "egress", "client", egressTicketStdinArg, "--id", "lease", "--session", "old"}, true},
		{"manual client", []string{egressRemoteBinary, "egress", "client", "--id", "lease", "--session", "old"}, true},
		{"equals flags", []string{egressRemoteBinary, "egress", "client", "--coordinator=https://broker.example", "--id=lease", "--session=old"}, true},
		{"single dash flags", []string{egressRemoteBinary, "egress", "client", "-id", "lease", "-session=old"}, true},
		{"positional lease", []string{egressRemoteBinary, "egress", "client", "--session", "old", "lease"}, true},
		{"successor", []string{egressRemoteBinary, "egress", "client", "--id", "lease", "--session", "old-new"}, false},
		{"other lease", []string{egressRemoteBinary, "egress", "client", "--id", "other", "--session", "old"}, false},
		{"host", []string{egressRemoteBinary, "egress", "host", "--id", "lease", "--session", "old"}, false},
		{"cleanup process", []string{egressRemoteBinary, "egress", "client", egressStopSessionArg, "--id", "lease", "--session", "old"}, false},
		{"substring", []string{egressRemoteBinary, "egress", "client", "--id", "lease", "--coordinator", "--session old"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := egressClientSessionArgsMatch(tc.args, "lease", "old"); got != tc.want {
				t.Fatalf("session process match = %t, want %t", got, tc.want)
			}
		})
	}
}
