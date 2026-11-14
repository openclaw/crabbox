package hetzner

import (
	"bytes"
	"encoding/json"

	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestPondConnectUnclaimedLease(t *testing.T) {
	for _, tc := range []struct {
		name               string
		claimOther, filter bool
	}{
		{name: "unclaimed"}, {name: "unclaimed provider filter", filter: true}, {name: "both claimed", claimOther: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dirs := testutil.IsolateUserDirs(t)
			t.Chdir(dirs.Root)
			mine := normalizeHetznerServer(core.Server{ID: 101, Name: "crabbox-mine", Labels: map[string]string{"crabbox": "true", "created_by": "crabbox", "provider": "hetzner", "state": "ready", "lease": "cbx_abcdef000001", "slug": "mine"}})
			other := normalizeHetznerServer(core.Server{ID: 102, Name: "crabbox-other", Labels: map[string]string{"crabbox": "true", "created_by": "crabbox", "provider": "hetzner", "state": "ready", "lease": "cbx_abcdef000002", "slug": "other"}})
			for i, s := range []*core.Server{&mine, &other} {
				s.Labels["pond"] = "alpha"
				s.Labels["crabbox_exposed_ports"] = []string{"8080", "5432"}[i]
				s.PublicNet.IPv4.IP = []string{"203.0.113.10", "203.0.113.20"}[i]
			}
			servers := []core.Server{mine, other}
			for i, s := range servers {
				if i == 1 && !tc.claimOther {
					continue
				}
				cfg := core.Config{Provider: "hetzner", Pond: "alpha"}
				if err := core.ClaimLeaseTargetForRepoConfig(s.Labels["lease"], s.Labels["slug"], cfg, s, core.SSHTarget{User: "runner", Host: s.PublicNet.IPv4.IP, Port: "22"}, dirs.Root, time.Hour, false); err != nil {
					t.Fatal(err)
				}
			}
			old := newHetznerClient
			t.Cleanup(func() { newHetznerClient = old })
			newHetznerClient = func() (hetznerClient, error) { return &fakeHetznerClient{list: servers}, nil }
			var out, stderr bytes.Buffer
			args := []string{"pond", "connect", "alpha", "--json"}
			if tc.filter {
				args = append(args, "--provider", "hetzner")
			}
			if err := (core.App{Stdout: &out, Stderr: &stderr}).Run(t.Context(), args); err != nil {
				t.Fatalf("connect: %v stderr=%s", err, stderr.String())
			}
			var summary struct {
				Forwards []struct {
					LeaseID string `json:"leaseID"`
				} `json:"forwards"`
			}
			if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
				t.Fatal(err)
			}
			want := 1
			if tc.claimOther {
				want = 2
			}
			if len(summary.Forwards) != want {
				t.Fatalf("forwards=%s want %d local members", out.String(), want)
			}
			for _, f := range summary.Forwards {
				if f.LeaseID == other.Labels["lease"] && !tc.claimOther {
					t.Errorf("unclaimed lease forwarded: %s", f.LeaseID)
				}
			}
		})
	}
}
