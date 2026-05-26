package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNormalizePondName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"alpha", "alpha"},
		{"  Alpha  ", "alpha"},
		{"alpha-bravo", "alpha-bravo"},
		{"Alpha_Bravo Charlie", "alpha-bravo-charlie"},
		{"---alpha---", "alpha"},
		{"alpha//bravo??", "alpha-bravo"},
		{"pond!!", "pond"},
		{"123-abc", "123-abc"},
	}
	for _, tc := range cases {
		if got := normalizePondName(tc.in); got != tc.want {
			t.Fatalf("normalizePondName(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestRequestedPondNameValidates(t *testing.T) {
	got, err := requestedPondName(" Alpha Pond ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "alpha-pond" {
		t.Fatalf("pond=%q want alpha-pond", got)
	}
	if got, err := requestedPondName(""); err != nil || got != "" {
		t.Fatalf("empty input got=%q err=%v", got, err)
	}
	if _, err := requestedPondName("!!"); err == nil {
		t.Fatalf("expected error for pond with no alnum")
	}
	tooLong := strings.Repeat("a", maxRequestedPondNameLength+1)
	if _, err := requestedPondName(tooLong); err == nil {
		t.Fatalf("expected error for pond %d chars", maxRequestedPondNameLength+1)
	}
}

func TestDirectLeaseLabelsRecordPond(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Class:       "standard",
		Profile:     "default",
		ProviderKey: "crabbox-cbx-abcdef123456",
		ServerType:  "cpx62",
		Pond:        "alpha",
		TTL:         15 * time.Minute,
		IdleTimeout: 4 * time.Minute,
	}
	labels := directLeaseLabels(cfg, "cbx_abcdef123456", "blue-lobster", "hetzner", "", true, now)
	if labels["pond"] != "alpha" {
		t.Fatalf("pond label=%q want alpha; full=%#v", labels["pond"], labels)
	}
}

func TestDirectLeaseLabelsOmitPondWhenEmpty(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		Class:       "standard",
		Profile:     "default",
		ProviderKey: "crabbox-cbx-abcdef123456",
		ServerType:  "cpx62",
		TTL:         15 * time.Minute,
		IdleTimeout: 4 * time.Minute,
	}
	labels := directLeaseLabels(cfg, "cbx_abcdef123456", "blue-lobster", "hetzner", "", true, now)
	if _, ok := labels["pond"]; ok {
		t.Fatalf("pond label should be omitted when cfg.Pond is empty; got %#v", labels)
	}
}

func TestFilterServersByPond(t *testing.T) {
	servers := []Server{
		{Name: "a", Labels: map[string]string{"pond": "alpha"}},
		{Name: "b", Labels: map[string]string{"pond": "bravo"}},
		{Name: "c", Labels: map[string]string{}},
		{Name: "d", Labels: map[string]string{"pond": "Alpha"}},
	}
	got := filterServersByPond(servers, "alpha")
	if len(got) != 2 {
		t.Fatalf("expected 2 servers for pond=alpha, got %d (%#v)", len(got), got)
	}
	if got[0].Name != "a" || got[1].Name != "d" {
		t.Fatalf("filtered set unexpected: %#v", got)
	}
	if same := filterServersByPond(servers, ""); len(same) != len(servers) {
		t.Fatalf("empty filter should be a no-op, got %d", len(same))
	}
	if none := filterServersByPond(servers, "charlie"); len(none) != 0 {
		t.Fatalf("expected zero matches for pond=charlie, got %d", len(none))
	}
}

func TestApplyLeaseCreateFlagsSetsPond(t *testing.T) {
	defaults := Config{
		Provider:    "hetzner",
		Profile:     "default",
		Class:       "standard",
		TargetOS:    targetLinux,
		TTL:         time.Hour,
		IdleTimeout: 15 * time.Minute,
		Network:     NetworkAuto,
		Capacity:    CapacityConfig{Market: "spot"},
	}
	fs := flag.NewFlagSet("warmup", flag.ContinueOnError)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := fs.Parse([]string{"--pond", "Alpha Pond"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatalf("applyLeaseCreateFlags: %v", err)
	}
	if cfg.Pond != "alpha-pond" {
		t.Fatalf("cfg.Pond=%q want alpha-pond", cfg.Pond)
	}
	opts := leaseOptionsFromConfig(cfg)
	if opts.Pond != "alpha-pond" {
		t.Fatalf("leaseOptionsFromConfig.Pond=%q want alpha-pond", opts.Pond)
	}
}

func TestApplyLeaseCreateFlagsRejectsBadPond(t *testing.T) {
	defaults := Config{
		Provider:    "hetzner",
		Profile:     "default",
		Class:       "standard",
		TargetOS:    targetLinux,
		TTL:         time.Hour,
		IdleTimeout: 15 * time.Minute,
		Network:     NetworkAuto,
		Capacity:    CapacityConfig{Market: "spot"},
	}
	fs := flag.NewFlagSet("warmup", flag.ContinueOnError)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := fs.Parse([]string{"--pond", "!!"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyLeaseCreateFlags(&cfg, fs, values); err == nil {
		t.Fatalf("expected error for invalid --pond")
	}
}

func TestFilterJSONListViewByPond(t *testing.T) {
	view := []any{
		map[string]any{"id": "a", "labels": map[string]any{"pond": "alpha"}},
		map[string]any{"id": "b", "labels": map[string]any{"pond": "bravo"}},
		map[string]any{"id": "c", "labels": map[string]any{}},
		map[string]any{"id": "d", "labels": map[string]any{"pond": "Alpha"}},
		"not-a-map",
	}
	filtered := filterJSONListViewByPond(view, "alpha")
	out, ok := filtered.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", filtered)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 entries, got %d (%#v)", len(out), out)
	}
	if same := filterJSONListViewByPond(view, ""); !sameAny(same, view) {
		t.Fatalf("empty filter should be identity")
	}
	if other := filterJSONListViewByPond("not-a-list", "alpha"); other != "not-a-list" {
		t.Fatalf("non-slice view should pass through unchanged, got %#v", other)
	}
	unsupported := []any{
		map[string]any{"id": "native-a", "state": "ready"},
		map[string]any{"id": "native-b", "state": "leased"},
	}
	if same := filterJSONListViewByPond(unsupported, "alpha"); fmt.Sprintf("%#v", same) != fmt.Sprintf("%#v", unsupported) {
		t.Fatalf("unlabeled JSON list should pass through unchanged, got %#v", same)
	}
}

func sameAny(a, b any) bool {
	as, aok := a.([]any)
	bs, bok := b.([]any)
	if !aok || !bok || len(as) != len(bs) {
		return false
	}
	for i := range as {
		if _, ok := as[i].(map[string]any); ok {
			continue
		}
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func TestPondTagOwnerTruncatesAndNormalizes(t *testing.T) {
	t.Run("short owner passes through", func(t *testing.T) {
		if got := pondTagOwner("a@b"); got != "a" {
			t.Fatalf("pondTagOwner short=%q want a", got)
		}
	})
	t.Run("empty owner empty out", func(t *testing.T) {
		for _, in := range []string{"", "!!"} {
			if got := pondTagOwner(in); got != "" {
				t.Fatalf("pondTagOwner(%q)=%q want empty", in, got)
			}
		}
	})
	t.Run("over-budget owner is stable hash, fixed length, lowercase alphanumeric", func(t *testing.T) {
		got := pondTagOwner("yossi.eliaz@incredibuild.com")
		if len(got) != maxPondTailscaleTagOwnerLength {
			t.Fatalf("pondTagOwner length=%d want %d", len(got), maxPondTailscaleTagOwnerLength)
		}
		for _, ch := range got {
			if !(('a' <= ch && ch <= 'z') || ('0' <= ch && ch <= '9')) {
				t.Fatalf("pondTagOwner=%q has non-alphanumeric byte %q", got, ch)
			}
		}
		// Determinism: repeated calls produce the same segment.
		if again := pondTagOwner("yossi.eliaz@incredibuild.com"); again != got {
			t.Fatalf("pondTagOwner not deterministic: %q vs %q", got, again)
		}
	})
	t.Run("distinct identities never collide (the truncation regression)", func(t *testing.T) {
		a := pondTagOwner("yossi.eliaz@incredibuild.com")
		b := pondTagOwner("yossi.elman@incredibuild.com")
		if a == b {
			t.Fatalf("two distinct identities produced the same owner segment %q — truncation regression", a)
		}
	})
}

func TestPondTailscaleTagShape(t *testing.T) {
	tag := pondTailscaleTag("yossi.eliaz@incredibuild.com", "Alpha Pond")
	if !strings.HasPrefix(tag, "tag:cbx-pond-") || !strings.HasSuffix(tag, "-alpha-pond") {
		t.Fatalf("pondTailscaleTag=%q want shape tag:cbx-pond-<owner>-alpha-pond", tag)
	}
	if got := pondTailscaleTag("", "alpha"); got != "tag:cbx-pond-user-alpha" {
		t.Fatalf("pondTailscaleTag empty owner=%q want tag:cbx-pond-user-alpha", got)
	}
	if got := pondTailscaleTag("yossi", ""); got != "" {
		t.Fatalf("pondTailscaleTag empty pond=%q want empty", got)
	}
}

func TestAppendPondTailscaleTag(t *testing.T) {
	cfg := Config{
		Pond: "alpha",
		Tailscale: TailscaleConfig{
			Enabled: true,
			Tags:    []string{"tag:crabbox"},
		},
	}
	appendPondTailscaleTag(&cfg, true)
	found := false
	for _, tag := range cfg.Tailscale.Tags {
		if strings.HasPrefix(tag, "tag:cbx-pond-") && strings.HasSuffix(tag, "-alpha") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected pond tag appended, got %#v", cfg.Tailscale.Tags)
	}
	// Idempotent: a second call must not duplicate the entry.
	before := len(cfg.Tailscale.Tags)
	appendPondTailscaleTag(&cfg, true)
	if len(cfg.Tailscale.Tags) != before {
		t.Fatalf("appendPondTailscaleTag duplicated entry; got %#v", cfg.Tailscale.Tags)
	}
	// No-op when Tailscale is not enabled.
	cfg2 := Config{Pond: "alpha", Tailscale: TailscaleConfig{Tags: []string{"tag:crabbox"}}}
	appendPondTailscaleTag(&cfg2, true)
	if len(cfg2.Tailscale.Tags) != 1 {
		t.Fatalf("expected no-op when Tailscale disabled, got %#v", cfg2.Tailscale.Tags)
	}
	// No-op when the provider does not advertise FeatureTailscale.
	cfg3 := Config{Pond: "alpha", Tailscale: TailscaleConfig{Enabled: true, Tags: []string{"tag:crabbox"}}}
	appendPondTailscaleTag(&cfg3, false)
	if len(cfg3.Tailscale.Tags) != 1 {
		t.Fatalf("expected no-op when provider lacks FeatureTailscale, got %#v", cfg3.Tailscale.Tags)
	}
}

func TestApplyLeaseCreateFlagsAddsPondTailscaleTag(t *testing.T) {
	defaults := Config{
		Provider:    "hetzner",
		Profile:     "default",
		Class:       "standard",
		TargetOS:    targetLinux,
		TTL:         time.Hour,
		IdleTimeout: 15 * time.Minute,
		Network:     NetworkAuto,
		Capacity:    CapacityConfig{Market: "spot"},
		Tailscale: TailscaleConfig{
			HostnameTemplate: "crabbox-{slug}",
			Tags:             []string{"tag:crabbox"},
		},
	}
	fs := flag.NewFlagSet("warmup", flag.ContinueOnError)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := fs.Parse([]string{"--pond", "alpha", "--tailscale"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatalf("applyLeaseCreateFlags: %v", err)
	}
	if !cfg.Tailscale.Enabled {
		t.Fatalf("expected Tailscale enabled by --tailscale")
	}
	hasPondTag := false
	for _, tag := range cfg.Tailscale.Tags {
		if strings.HasPrefix(tag, "tag:cbx-pond-") && strings.HasSuffix(tag, "-alpha") {
			hasPondTag = true
		}
	}
	if !hasPondTag {
		t.Fatalf("expected pond Tailscale tag on cfg, got %#v", cfg.Tailscale.Tags)
	}
}

func TestApplyLeaseCreateFlagsKeepsBrokeredPondTailscaleTagsAllowlisted(t *testing.T) {
	t.Setenv("TS_API_KEY", "tskey-test")
	t.Setenv("CRABBOX_TAILSCALE_AUTH_KEY", "tskey-auth-test")
	var factoryCalls int
	prev := pondTailnetACLClientFactory
	pondTailnetACLClientFactory = func(_ string) pondTailnetACLClient {
		factoryCalls++
		return &stubPondTailnetACLClient{policy: `{"tagOwners":{}}`, etag: `"v1"`}
	}
	t.Cleanup(func() { pondTailnetACLClientFactory = prev })

	defaults := Config{
		Provider:    "hetzner",
		Profile:     "default",
		Class:       "standard",
		TargetOS:    targetLinux,
		Coordinator: "https://broker.example.test",
		TTL:         time.Hour,
		IdleTimeout: 15 * time.Minute,
		Network:     NetworkAuto,
		Capacity:    CapacityConfig{Market: "spot"},
		Tailscale: TailscaleConfig{
			HostnameTemplate: "crabbox-{slug}",
			Tags:             []string{"tag:crabbox"},
		},
	}
	fs := flag.NewFlagSet("warmup", flag.ContinueOnError)
	values := registerLeaseCreateFlags(fs, defaults)
	if err := fs.Parse([]string{"--pond", "alpha", "--tailscale"}); err != nil {
		t.Fatal(err)
	}
	cfg := defaults
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatalf("applyLeaseCreateFlags: %v", err)
	}
	if !cfg.Tailscale.Enabled {
		t.Fatalf("expected Tailscale enabled by --tailscale")
	}
	for _, tag := range cfg.Tailscale.Tags {
		if strings.HasPrefix(tag, "tag:cbx-pond-") {
			t.Fatalf("brokered lease must not send dynamic pond tag to Worker allowlist: %#v", cfg.Tailscale.Tags)
		}
	}
	if factoryCalls != 0 {
		t.Fatalf("brokered lease should not auto-bootstrap direct pond ACL, factory calls=%d", factoryCalls)
	}
}

func TestCloudInitPondHostsBootstrapEmittedWhenPondAndTailscale(t *testing.T) {
	cfg := baseConfig()
	cfg.SSHUser = "runner"
	cfg.Pond = "alpha"
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "tskey-secret"
	cfg.Tailscale.Tags = []string{"tag:crabbox", "tag:cbx-pond-user-alpha"}
	got := cloudInit(cfg, "ssh-ed25519 test")
	for _, want := range []string{
		"/etc/hosts.cbx",
		"/usr/local/bin/crabbox-pond-hosts",
		"/etc/systemd/system/crabbox-pond-hosts.service",
		"/etc/systemd/system/crabbox-pond-hosts.timer",
		"/etc/hosts",
		"# crabbox pond hosts begin",
		"# crabbox pond hosts end",
		"OnUnitActiveSec=30s",
		"ExecStart=/usr/local/bin/crabbox-pond-hosts",
		"tag:cbx-pond-",
		"tailscale status --json",
		"systemctl enable --now crabbox-pond-hosts.timer",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloudInit(pond) missing %q", want)
		}
	}
}

func TestCloudInitPondHostsBootstrapAbsentWithoutPond(t *testing.T) {
	cfg := baseConfig()
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "tskey-secret"
	got := cloudInit(cfg, "ssh-ed25519 test")
	if strings.Contains(got, "crabbox-pond-hosts") {
		t.Fatalf("cloudInit without --pond must not install pond hosts service")
	}
}

func TestCloudInitPondHostsBootstrapAbsentWithoutTailscale(t *testing.T) {
	cfg := baseConfig()
	cfg.Pond = "alpha"
	got := cloudInit(cfg, "ssh-ed25519 test")
	if strings.Contains(got, "crabbox-pond-hosts") {
		t.Fatalf("cloudInit without --tailscale must not install pond hosts service even when --pond is set")
	}
}

// stubDoctorTailscaleACLClient lets unit tests exercise doctorPondSummary
// without touching the real Tailscale API.
type stubDoctorTailscaleACLClient struct {
	policy string
	err    error
}

func (s stubDoctorTailscaleACLClient) PolicyHuJSON(_ context.Context, _ string) (string, error) {
	return s.policy, s.err
}

func TestDoctorPondSummaryNoopsWithoutPond(t *testing.T) {
	cfg := Config{Provider: "hetzner"}
	status, message, details := doctorPondSummary(context.Background(), cfg)
	if status != "" || message != "" || details != nil {
		t.Fatalf("expected no pond check without cfg.Pond, got status=%q msg=%q details=%#v", status, message, details)
	}
}

func TestDoctorPondSummarySkipsWithoutLocalPondClaims(t *testing.T) {
	cfg := Config{Provider: "e2b", Pond: "alpha"}
	status, message, details := doctorPondSummary(context.Background(), cfg)
	if status != "skip" {
		t.Fatalf("expected skip, got %q", status)
	}
	want := `pond "alpha": no local lease claims found; create or claim a pond member before checking Tailscale policy`
	if message != want {
		t.Fatalf("verdict text drifted\n want: %q\n got:  %q", want, message)
	}
	if details["reason"] != "no_pond_claims" {
		t.Fatalf("expected reason metadata, got %#v", details)
	}
}

func TestDoctorPondSummarySkipsPondWithOnlyNonTailscaleClaims(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_e2b1", Provider: "e2b", Pond: "alpha", Slug: "api", RepoRoot: "/r"},
	})
	t.Setenv("TS_API_KEY", "tskey-api-stub")
	prev := doctorTailscaleACLClientFactory
	called := false
	defer func() { doctorTailscaleACLClientFactory = prev }()
	doctorTailscaleACLClientFactory = func(_ string) doctorTailscaleACLClient {
		called = true
		return stubDoctorTailscaleACLClient{policy: pondPolicyFixture(pondTailscaleTag(localCoordinatorOwner(), "alpha"))}
	}
	cfg := Config{Provider: "hetzner", Pond: "alpha"}
	status, _, details := doctorPondSummary(context.Background(), cfg)
	if status != "skip" {
		t.Fatalf("expected skip for URL-only pond, got %q details=%#v", status, details)
	}
	if called {
		t.Fatalf("doctor should not call Tailscale policy API for a pond with only non-Tailscale claims")
	}
	if details["reason"] != "no_tailscale_capable_provider" {
		t.Fatalf("expected no_tailscale_capable_provider reason, got %#v", details)
	}
}

func TestDoctorPondSummarySkipsWhenAPIKeyMissing(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_hz1", Provider: "hetzner", Pond: "alpha", Slug: "web", RepoRoot: "/r"},
	})
	t.Setenv("TS_API_KEY", "")
	cfg := Config{Provider: "hetzner", Pond: "alpha"}
	status, _, details := doctorPondSummary(context.Background(), cfg)
	if status != "skip" {
		t.Fatalf("expected skip when TS_API_KEY is missing, got %q", status)
	}
	if details["reason"] != "ts_api_key_missing" {
		t.Fatalf("expected ts_api_key_missing reason, got %#v", details)
	}
}

func TestDoctorPondSummaryOKWhenACLPresent(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_hz1", Provider: "hetzner", Pond: "alpha", Slug: "web", RepoRoot: "/r"},
	})
	t.Setenv("TS_API_KEY", "tskey-api-stub")
	tag := pondTailscaleTag(localCoordinatorOwner(), "alpha")
	policy := pondPolicyFixture(tag)
	prev := doctorTailscaleACLClientFactory
	defer func() { doctorTailscaleACLClientFactory = prev }()
	doctorTailscaleACLClientFactory = func(_ string) doctorTailscaleACLClient {
		return stubDoctorTailscaleACLClient{policy: policy}
	}
	cfg := Config{Provider: "hetzner", Pond: "alpha"}
	status, message, details := doctorPondSummary(context.Background(), cfg)
	if status != "ok" {
		t.Fatalf("expected ok, got %q (msg=%q details=%#v)", status, message, details)
	}
	wantTag := pondTailscaleTag(localCoordinatorOwner(), "alpha")
	want := fmt.Sprintf(`pond "alpha": Tailscale plane auto-managed (%s)`, wantTag)
	if message != want {
		t.Fatalf("verdict text drifted\n want: %q\n got:  %q", want, message)
	}
	if details["mode"] != "auto-managed" {
		t.Fatalf("expected mode=auto-managed when TS_API_KEY is set, got %#v", details)
	}
}

func TestDoctorPondSummaryFailsWhenACLMissing(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_hz1", Provider: "hetzner", Pond: "alpha", Slug: "web", RepoRoot: "/r"},
	})
	t.Setenv("TS_API_KEY", "tskey-api-stub")
	policy := pondPolicyFixture("tag:cbx-pond-user-bravo")
	prev := doctorTailscaleACLClientFactory
	defer func() { doctorTailscaleACLClientFactory = prev }()
	doctorTailscaleACLClientFactory = func(_ string) doctorTailscaleACLClient {
		return stubDoctorTailscaleACLClient{policy: policy}
	}
	cfg := Config{Provider: "hetzner", Pond: "alpha"}
	status, message, details := doctorPondSummary(context.Background(), cfg)
	if status != "failed" {
		t.Fatalf("expected failed, got %q", status)
	}
	wantTag := pondTailscaleTag(localCoordinatorOwner(), "alpha")
	want := fmt.Sprintf(`pond "alpha": tailnet policy row missing for %s. Run with $TS_API_KEY exported to auto-install, or apply the snippet from docs/features/pond.md`, wantTag)
	if message != want {
		t.Fatalf("verdict text drifted\n want: %q\n got:  %q", want, message)
	}
	if details["remedy"] == "" {
		t.Fatalf("expected remedy in details, got %#v", details)
	}
}

func TestDoctorPondSummaryFailsWhenAPIClientErrors(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_hz1", Provider: "hetzner", Pond: "alpha", Slug: "web", RepoRoot: "/r"},
	})
	t.Setenv("TS_API_KEY", "tskey-api-stub")
	prev := doctorTailscaleACLClientFactory
	defer func() { doctorTailscaleACLClientFactory = prev }()
	doctorTailscaleACLClientFactory = func(_ string) doctorTailscaleACLClient {
		return stubDoctorTailscaleACLClient{err: fmt.Errorf("tailscale api 401: invalid api key")}
	}
	cfg := Config{Provider: "hetzner", Pond: "alpha"}
	status, message, _ := doctorPondSummary(context.Background(), cfg)
	if status != "failed" {
		t.Fatalf("expected failed, got %q", status)
	}
	if !strings.Contains(message, "policy lookup failed") {
		t.Fatalf("expected policy lookup failure message, got %q", message)
	}
}

// TestDoctorPondSummarySkipsWhenControlPlaneIncompatible covers self-hosted
// control planes (e.g. Headscale) whose policy endpoint is not byte-compatible
// with Tailscale's /api/v2/tailnet/.../acl shape. Doctor must skip with a
// helpful pointer at the manual snippet rather than reporting a failure.
func TestDoctorPondSummarySkipsWhenControlPlaneIncompatible(t *testing.T) {
	withTempClaims(t, []leaseClaim{
		{LeaseID: "cbx_hz1", Provider: "hetzner", Pond: "alpha", Slug: "web", RepoRoot: "/r"},
	})
	t.Setenv("TS_API_KEY", "tskey-api-stub")
	t.Setenv("CRABBOX_TS_API_URL", "https://headscale.example.com")
	t.Setenv("TS_API_URL", "")
	prev := doctorTailscaleACLClientFactory
	defer func() { doctorTailscaleACLClientFactory = prev }()
	doctorTailscaleACLClientFactory = func(_ string) doctorTailscaleACLClient {
		return stubDoctorTailscaleACLClient{err: fmt.Errorf("%w: GET https://headscale.example.com/api/v2/tailnet/-/acl returned 404", ErrPondACLAutoBootstrapUnavailable)}
	}
	cfg := Config{Provider: "hetzner", Pond: "alpha"}
	status, message, details := doctorPondSummary(context.Background(), cfg)
	if status != "skip" {
		t.Fatalf("expected skip on incompatible control plane, got %q (msg=%q)", status, message)
	}
	if !strings.Contains(message, "https://headscale.example.com") {
		t.Fatalf("expected control plane URL in message, got %q", message)
	}
	if !strings.Contains(message, "docs/features/pond.md") {
		t.Fatalf("expected manual snippet pointer, got %q", message)
	}
	if details["reason"] != "control_plane_incompatible" {
		t.Fatalf("expected control_plane_incompatible reason, got %#v", details)
	}
	if details["api_url"] != "https://headscale.example.com" {
		t.Fatalf("expected api_url metadata, got %#v", details)
	}
}

func TestPondACLRowPresentChecksConcreteTag(t *testing.T) {
	tag := "tag:cbx-pond-yossi-e-alpha"
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"both present", pondPolicyFixture(tag), true},
		{"grants present", pondGrantPolicyFixture(tag), true},
		{"commented sample before grants", `{
  // "tagOwners": { "tag:cbx-pond-yossi-e-alpha": ["autogroup:admin"] },
  "tagOwners": { "tag:cbx-pond-yossi-e-alpha": ["autogroup:admin"] },
  "grants": [{ "src": ["tag:cbx-pond-yossi-e-alpha"], "dst": ["tag:cbx-pond-yossi-e-alpha"], "ip": ["*"] }]
}`, true},
		{"commented tag owner only", `{
  // "tagOwners": { "tag:cbx-pond-yossi-e-alpha": ["autogroup:admin"] },
  "grants": [{ "src": ["tag:cbx-pond-yossi-e-alpha"], "dst": ["tag:cbx-pond-yossi-e-alpha"], "ip": ["*"] }]
}`, false},
		{"multiline trailing commas", `{
  "tagOwners": {
    "tag:cbx-pond-yossi-e-alpha": ["autogroup:admin"],
  },
  "grants": [
    {
      "src": ["tag:cbx-pond-yossi-e-alpha",],
      "dst": ["tag:cbx-pond-yossi-e-alpha",],
      "ip": ["*",],
    },
  ],
}`, true},
		{"different pond", pondPolicyFixture("tag:cbx-pond-yossi-e-bravo"), false},
		{"missing tagOwners", `{"acls":[{"src":["tag:cbx-pond-yossi-e-alpha"],"dst":["tag:cbx-pond-yossi-e-alpha:*"]}]}`, false},
		{"missing dst", `{"tagOwners":{"tag:cbx-pond-yossi-e-alpha":["autogroup:admin"]},"acls":[{"src":["tag:cbx-pond-yossi-e-alpha"],"dst":["tag:crabbox:*"]}]}`, false},
		{"grant src only", `{"tagOwners":{"tag:cbx-pond-yossi-e-alpha":["autogroup:admin"]},"grants":[{"src":["tag:cbx-pond-yossi-e-alpha"],"dst":["tag:crabbox"],"ip":["*"]}]}`, false},
		{"missing tag mention", `{"acls":[{"src":["*"]}],"tagOwners":{"tag:crabbox":["autogroup:admin"]}}`, false},
		{"empty body", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pondACLRowPresent(tc.body, tag); got != tc.want {
				t.Fatalf("pondACLRowPresent(%q)=%v want %v", tc.name, got, tc.want)
			}
		})
	}
}

func pondPolicyFixture(tag string) string {
	return fmt.Sprintf(`{
  "tagOwners": { %q: ["autogroup:admin"] },
  "acls": [{ "action": "accept", "src": [%q], "dst": [%q] }]
}`, tag, tag, tag+":*")
}

func pondGrantPolicyFixture(tag string) string {
	return fmt.Sprintf(`{
  "tagOwners": { %q: ["autogroup:admin"] },
  "grants": [{ "src": [%q], "dst": [%q], "ip": ["*"] }]
}`, tag, tag, tag)
}
