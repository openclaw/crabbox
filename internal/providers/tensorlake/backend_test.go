package tensorlake

import (
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpec(t *testing.T) {
	p := Provider{}
	if p.Name() != "tensorlake" {
		t.Fatalf("Name=%q want tensorlake", p.Name())
	}
	aliases := p.Aliases()
	if len(aliases) == 0 {
		t.Fatalf("expected aliases, got none")
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindDelegatedRun {
		t.Fatalf("kind=%v want delegated run", spec.Kind)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v want never", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v want [{linux}]", spec.Targets)
	}
}

func TestBuildCommandShellMode(t *testing.T) {
	got, err := buildCommand([]string{"pnpm install && pnpm test"}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bash", "-lc", "pnpm install && pnpm test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command=%#v want %#v", got, want)
	}
}

func TestBuildCommandPassThrough(t *testing.T) {
	got, err := buildCommand([]string{"pnpm", "test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pnpm", "test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command=%#v want %#v", got, want)
	}
}

func TestBuildCommandRejectsEmpty(t *testing.T) {
	if _, err := buildCommand(nil, false); err == nil {
		t.Fatalf("expected error for empty command")
	}
}

func TestIsCrabboxSandboxName(t *testing.T) {
	cases := map[string]bool{
		"crabbox-foo-abc123": true,
		"crabbox-x-1":        true,
		"my-app-001":         false,
		"":                   false,
	}
	for name, want := range cases {
		if got := isCrabboxSandboxName(name); got != want {
			t.Errorf("isCrabboxSandboxName(%q)=%v want %v", name, got, want)
		}
	}
}

func TestResolveLeaseIDRejectsForeignSandboxes(t *testing.T) {
	_, _, err := resolveLeaseID("not-a-crabbox-sandbox", "", false, 0)
	if err == nil || !strings.Contains(err.Error(), "not claimed by Crabbox") {
		t.Fatalf("err=%v, want rejection of unclaimed sandbox", err)
	}
}

func TestResolveLeaseIDAcceptsLeasePrefix(t *testing.T) {
	lease, name, err := resolveLeaseID("tlsbx_crabbox-app-aaa111", "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if lease != "tlsbx_crabbox-app-aaa111" || name != "crabbox-app-aaa111" {
		t.Fatalf("lease=%q name=%q", lease, name)
	}
}

func TestResolveLeaseIDRejectsForeignLeasePrefix(t *testing.T) {
	_, _, err := resolveLeaseID("tlsbx_someone-elses-sandbox", "", false, 0)
	if err == nil || !strings.Contains(err.Error(), "not a Crabbox-owned sandbox") {
		t.Fatalf("err=%v, want rejection", err)
	}
}

func TestResolveLeaseIDRequiresIdentifier(t *testing.T) {
	if _, _, err := resolveLeaseID("", "", false, 0); err == nil {
		t.Fatalf("expected error for empty id")
	}
}

func TestNewSandboxNameUsesRepoName(t *testing.T) {
	repo := Repo{Name: "carbbox"}
	name := newSandboxName(repo)
	if !strings.HasPrefix(name, "crabbox-carbbox-") {
		t.Fatalf("name=%q does not start with crabbox-carbbox-", name)
	}
	if !isCrabboxSandboxName(name) {
		t.Fatalf("isCrabboxSandboxName(%q)=false", name)
	}
}

func TestNewSandboxNameStripsRedundantPrefix(t *testing.T) {
	repo := Repo{Name: "crabbox-app"}
	name := newSandboxName(repo)
	if strings.HasPrefix(name, "crabbox-crabbox-") {
		t.Fatalf("name=%q double-prefixed", name)
	}
	if !strings.HasPrefix(name, "crabbox-app-") {
		t.Fatalf("name=%q does not start with crabbox-app-", name)
	}
}
