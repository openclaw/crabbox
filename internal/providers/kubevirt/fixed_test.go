package kubevirt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"gopkg.in/yaml.v3"
)

// Records Kubernetes API requests. Readiness intentionally fails after create,
// exercising durable identity recovery without simulating guest SSH success.
type lifecycleRunner struct {
	item                map[string]any
	creates             int
	deletes             []string
	replacementAtDelete bool
	allowPatch          bool
	loseCreateACK       bool
}

func (r *lifecycleRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	args := req.Args
	for len(args) > 0 && args[0] != "get" && args[0] != "create" && args[0] != "apply" && args[0] != "patch" && args[0] != "delete" && args[0] != "start" {
		args = args[1:]
	}
	if len(args) == 0 {
		return core.LocalCommandResult{}, fmt.Errorf("unexpected API request: %v", req.Args)
	}
	out := func(v any) (core.LocalCommandResult, error) {
		data, e := json.Marshal(v)
		return core.LocalCommandResult{Stdout: string(data)}, e
	}
	failure := func(s string) (core.LocalCommandResult, error) {
		return core.LocalCommandResult{ExitCode: 1, Stderr: s}, fmt.Errorf("%s", s)
	}
	switch args[0] {
	case "get":
		if args[1] == "namespace" {
			return out(map[string]any{"metadata": map[string]string{"uid": "namespace-original"}})
		}
		if strings.Contains(args[1], "/") {
			if r.item == nil {
				return failure("NotFound")
			}
			return out(r.item)
		}
		items := []any{}
		if r.item != nil {
			items = append(items, r.item)
		}
		return out(map[string]any{"items": items})
	case "create", "apply":
		r.creates++
		var path string
		for i, a := range args {
			if a == "-f" {
				path = args[i+1]
			}
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return core.LocalCommandResult{}, e
		}
		var doc map[string]any
		if e = yaml.Unmarshal(data, &doc); e != nil {
			return core.LocalCommandResult{}, e
		}
		doc["metadata"].(map[string]any)["uid"] = "vm-original"
		r.item = doc
		if r.loseCreateACK {
			return failure("creation response lost")
		}
		return out(doc)
	case "patch", "start":
		if r.allowPatch {
			return out(map[string]any{})
		}
		return failure("injected readiness failure after create acknowledgment")
	case "delete":
		var path string
		for i, a := range args {
			if a == "-f" {
				path = args[i+1]
			}
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return core.LocalCommandResult{}, e
		}
		var opts struct {
			Preconditions struct {
				UID string `json:"uid"`
			} `json:"preconditions"`
		}
		if e = json.Unmarshal(data, &opts); e != nil {
			return core.LocalCommandResult{}, e
		}
		r.deletes = append(r.deletes, opts.Preconditions.UID)
		if r.replacementAtDelete {
			r.item["metadata"].(map[string]any)["uid"] = "replacement"
		}
		if r.item != nil && opts.Preconditions.UID != r.item["metadata"].(map[string]any)["uid"] {
			return failure("Conflict: UID precondition failed")
		}
		r.item = nil
		return out(map[string]any{})
	}
	return failure("unexpected API verb")
}

func boundLifecycle(t *testing.T) (*leaseBackend, *lifecycleRunner, core.AcquireRequest, core.LeaseClaim) {
	t.Helper()
	isolateCrabboxState(t)
	cfg := testConfig(t)
	r := &lifecycleRunner{}
	b := &leaseBackend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: r}}
	req := core.AcquireRequest{RequestedLeaseID: "cbx_123456789abc", RequestedSlug: "unit", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := b.Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "injected readiness") {
		t.Fatalf("expected post-create readiness failure, got %v", err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.CloudImmutableID != "vm-original" || claim.CloudID == "" {
		t.Fatalf("creation acknowledgment not durably bound: claim=%+v err=%v", claim, err)
	}
	return b, r, req, claim
}

func TestFixedKubeVirtReplayAndTerminalID(t *testing.T) {
	b, r, req, claim := boundLifecycle(t)
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("injected readiness failure disappeared")
	}
	if r.creates != 1 {
		t.Fatalf("fixed replay created %d VMs", r.creates)
	}
	// A changed template is a conflicting intent, even while readiness is failed.
	original, err := os.ReadFile(b.cfg.KubeVirt.Template)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(b.cfg.KubeVirt.Template, append(original, []byte("\n# changed intent\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("changed intent accepted: %v", err)
	}
	if err = os.WriteFile(b.cfg.KubeVirt.Template, original, 0600); err != nil {
		t.Fatal(err)
	}
	lease := core.LeaseTarget{LeaseID: claim.LeaseID, Server: b.itemToServer(r.vm(t))}
	if outcome, err := b.ReleaseLeaseWithOutcome(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil || !outcome.Terminal {
		t.Fatalf("release outcome=%+v err=%v", outcome, err)
	}
	if len(r.deletes) != 1 || r.deletes[0] != "vm-original" {
		t.Fatalf("delete lacked original UID precondition: %v", r.deletes)
	}
	if _, err = b.Acquire(t.Context(), req); err == nil {
		t.Fatal("terminal ID recreated")
	}
	if r.creates != 1 {
		t.Fatal("ended ID allocated another VM")
	}
	terminal, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || terminal.FixedCreateIntent == nil || terminal.FixedCreateIntent.State != "released" {
		t.Fatalf("terminal journal lost: %+v %v", terminal, err)
	}
}

func TestKubeVirtDeletionFencesReplacementRace(t *testing.T) {
	b, r, _, claim := boundLifecycle(t)
	r.replacementAtDelete = true
	lease := core.LeaseTarget{LeaseID: claim.LeaseID, Server: b.itemToServer(r.vm(t))}
	if outcome, err := b.ReleaseLeaseWithOutcome(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || outcome.Terminal {
		t.Fatalf("replacement race reported terminal success: %+v %v", outcome, err)
	}
	if r.item == nil || r.item["metadata"].(map[string]any)["uid"] != "replacement" {
		t.Fatal("same-name replacement was deleted")
	}
	if len(r.deletes) != 1 || r.deletes[0] != "vm-original" {
		t.Fatalf("UID precondition missing: %v", r.deletes)
	}
	if _, err := b.ReleaseLeaseWithOutcome(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("retry adopted replacement UID")
	}
	if len(r.deletes) != 1 {
		t.Fatalf("retry sent deletion to replacement: %v", r.deletes)
	}
}

func (r *lifecycleRunner) vm(t *testing.T) kubeVirtItem {
	t.Helper()
	data, err := json.Marshal(r.item)
	if err != nil {
		t.Fatal(err)
	}
	var item kubeVirtItem
	if err = json.Unmarshal(data, &item); err != nil {
		t.Fatal(err)
	}
	return item
}

func TestKubeVirtUnacknowledgedCreateIsNotAdoptedOrRepeated(t *testing.T) {
	isolateCrabboxState(t)
	cfg := testConfig(t)
	runner := &lifecycleRunner{loseCreateACK: true}
	backend := &leaseBackend{cfg: cfg, rt: core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}}
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef012345", RequestedSlug: "uncertain", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := backend.Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "creation response lost") {
		t.Fatalf("lost response not preserved: %v", err)
	}
	if _, err := backend.Acquire(t.Context(), req); err == nil {
		t.Fatal("copied creation labels authorized UID adoption")
	}
	if runner.creates != 1 {
		t.Fatalf("uncertain creation repeated %d times", runner.creates)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent == nil || claim.CloudImmutableID != "" {
		t.Fatalf("uncertain intent not retained: %+v %v", claim, err)
	}
	lease := core.LeaseTarget{LeaseID: req.RequestedLeaseID, Server: backend.itemToServer(runner.vm(t))}
	if _, err := backend.ReleaseLeaseWithOutcome(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("unbound VM deletion authorized")
	}
	if len(runner.deletes) != 0 {
		t.Fatal("sent deletion without acknowledged UID")
	}
}

func TestKubeVirtMissingBoundVMRelease(t *testing.T) {
	backend, runner, req, _ := boundLifecycle(t)
	runner.item = nil
	lease, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := backend.ReleaseLeaseWithOutcome(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil || !outcome.Terminal {
		t.Fatalf("absent bound UID release: %+v %v", outcome, err)
	}
	if runner.creates != 1 || len(runner.deletes) != 0 {
		t.Fatal("absence release issued a create or delete")
	}
}
