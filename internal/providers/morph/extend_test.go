package morph

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestMorphExtendNativeDeadlineBeforeMetadata(t *testing.T) {
	for _, failure := range []string{"none", "native", "metadata"} {
		t.Run(failure, func(t *testing.T) {
			configureMorphTestHome(t)
			now := time.Now().UTC().Truncate(time.Second)
			cfg := testMorphConfig()
			cfg.TTL, cfg.IdleTimeout = 30*time.Minute, 30*time.Minute
			const id = "cbx_morphextend"
			labels := core.DirectLeaseLabels(cfg, id, "extend", providerName, "", true, now.Add(-20*time.Minute))
			labels["last_touched_at"] = core.LeaseLabelTime(now)
			labels["state"] = "ready"
			instance := morphInstance{ID: "inst_extend", Status: "ready", Metadata: morphMetadata(labels)}
			server := core.Server{Provider: providerName, CloudID: instance.ID, Labels: labels}
			if err := core.ClaimLeaseTargetForRepoConfig(id, "extend", cfg, server, core.SSHTarget{}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
				t.Fatal(err)
			}
			original, err := core.ReadLeaseClaim(id)
			if err != nil {
				t.Fatal(err)
			}
			core.SetServerLeaseClaimSnapshot(&server, original, true)
			var order []string
			var ttls []int
			fail := failure
			sentinel := errors.New("synthetic provider failure")
			fake := &fakeMorphAPI{
				getInstance: func(context.Context, string) (morphInstance, error) { return instance, nil },
				updateInstanceTTL: func(_ context.Context, _ string, seconds int, _ string) error {
					order = append(order, "native")
					ttls = append(ttls, seconds)
					if fail == "native" {
						return sentinel
					}
					return nil
				},
				setInstanceMetadata: func(_ context.Context, _ string, next map[string]string) error {
					order = append(order, "metadata")
					if fail == "metadata" {
						return sentinel
					}
					instance.Metadata = morphMetadata(next)
					return nil
				},
			}
			b := &morphLeaseBackend{spec: Provider{}.Spec(), cfg: cfg, client: fake, now: func() time.Time { return now }}
			req := core.LeaseExtendRequest{Lease: core.LeaseTarget{LeaseID: id, Server: server}, By: 30 * time.Minute}
			_, err = b.ExtendLease(t.Context(), req)
			if failure != "none" {
				if !errors.Is(err, sentinel) {
					t.Fatalf("error=%v", err)
				}
				unchanged, readErr := core.ReadLeaseClaim(id)
				if readErr != nil || !reflect.DeepEqual(unchanged, original) || instance.Metadata["ttl_secs"] != "1800" {
					t.Fatalf("failed extension published state: %+v %v", unchanged, readErr)
				}
				if failure == "native" && !reflect.DeepEqual(order, []string{"native"}) {
					t.Fatalf("metadata written before native deadline: %v", order)
				}
				fail = "none"
				_, err = b.ExtendLease(t.Context(), req)
			}
			if err != nil {
				t.Fatal(err)
			}
			saved, err := core.ReadLeaseClaim(id)
			if err != nil || saved.Labels["ttl_secs"] != "3600" || instance.Metadata["ttl_secs"] != "3600" {
				t.Fatalf("extension=%+v err=%v", saved, err)
			}
			for _, seconds := range ttls {
				if seconds != 1800 {
					t.Fatalf("retry extended twice: native TTLs=%v", ttls)
				}
			}
			want := []string{"native", "metadata"}
			if failure == "native" {
				want = []string{"native", "native", "metadata"}
			}
			if failure == "metadata" {
				want = []string{"native", "metadata", "native", "metadata"}
			}
			if !reflect.DeepEqual(order, want) {
				t.Fatalf("order=%v want=%v", order, want)
			}
		})
	}
}
