package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarketplaceStatusCommandPrintsGatewayState(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	var sawAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/marketplace/status" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		sawAuth = r.Header.Get("Authorization") == "Bearer test-token"
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"marketplace":{"enabled":true,"mode":"preview","currency":"USD","creditUnit":"usd","requireCreditsForLeases":false,"supportedProviders":["aws","hetzner"],"features":{"quotes":true,"bidding":true,"payments":false,"creditLedger":false,"leaseEnforcement":false},"settlement":{"paymentProvider":"none","ledgerProvider":"none","providerSettlement":"external"},"decisionsRequired":["choose payment processor"]},"owner":"alice@example.com","org":"example-org"}`))
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "test-token")

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run(context.Background(), []string{"marketplace", "status"}); err != nil {
		t.Fatal(err)
	}
	if !sawAuth {
		t.Fatal("marketplace status did not send coordinator token")
	}
	output := stdout.String()
	for _, want := range []string{
		"marketplace mode=preview enabled=true",
		"identity owner=alice@example.com org=example-org",
		"features quotes=on bidding=on payments=off ledger=off enforcement=off",
		"providers aws,hetzner",
		"choose payment processor",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestMarketplaceQuoteCommandSendsSmartRoutingIntent(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	var body CoordinatorMarketplaceQuoteRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/marketplace/quotes" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"quote":{"id":"mq_123","mode":"preview","currency":"USD","creditUnit":"usd","strategy":"cheapest","ttlSeconds":7200,"selected":{"provider":"hetzner","target":"linux","class":"beast","serverType":"beast","priority":10,"weight":1,"ttlSeconds":7200,"costHourlyUSD":1,"retailHourlyUSD":1.5,"estimatedCostUSD":2,"credits":3,"marginUSD":1,"routeKey":"hetzner:linux:beast","available":true},"candidates":[{"provider":"hetzner","target":"linux","class":"beast","serverType":"beast","priority":10,"weight":1,"ttlSeconds":7200,"costHourlyUSD":1,"retailHourlyUSD":1.5,"estimatedCostUSD":2,"credits":3,"marginUSD":1,"routeKey":"hetzner:linux:beast","available":true}],"warnings":["preview quote only"]},"marketplace":{"enabled":true,"mode":"preview","currency":"USD","creditUnit":"usd","requireCreditsForLeases":false,"supportedProviders":["aws","hetzner"],"features":{"quotes":true,"bidding":true,"payments":false,"creditLedger":false,"leaseEnforcement":false},"settlement":{"paymentProvider":"none","ledgerProvider":"none","providerSettlement":"external"},"decisionsRequired":["choose ledger"]}}`))
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "test-token")

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	err := app.Run(context.Background(), []string{
		"marketplace",
		"quote",
		"--provider",
		"auto",
		"--providers",
		"aws,hetzner",
		"--class",
		"beast",
		"--ttl",
		"2h",
		"--max-credits",
		"5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if body.Provider != "auto" || body.Class != "beast" || body.TTLSeconds != 7200 || body.MaxCredits != 5 {
		t.Fatalf("unexpected quote body: %#v", body)
	}
	if got := strings.Join(body.Providers, ","); got != "aws,hetzner" {
		t.Fatalf("providers=%q", got)
	}
	output := stdout.String()
	for _, want := range []string{
		"marketplace quote mq_123 mode=preview strategy=cheapest ttl=2h0m0s",
		"selected provider=hetzner route=hetzner:linux:beast priority=10 weight=1.00 credits=$3.00",
		"preview quote only",
		"choose ledger",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	// a cheapest quote carries no routeShare/routingPlan, so no share suffix or plan should render
	if strings.Contains(output, "share=") {
		t.Fatalf("cheapest quote should not render a route share:\n%s", output)
	}
	if strings.Contains(output, "routing plan") {
		t.Fatalf("cheapest quote should not render a routing plan:\n%s", output)
	}
}

func TestMarketplaceQuoteCommandRendersWeightedRouteShare(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	var body CoordinatorMarketplaceQuoteRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/marketplace/quotes" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"quote":{"id":"mq_w","mode":"preview","currency":"USD","creditUnit":"usd","strategy":"weighted","ttlSeconds":3600,"selected":{"provider":"aws","target":"linux","class":"beast","serverType":"beast","priority":10,"weight":3,"ttlSeconds":3600,"costHourlyUSD":2,"retailHourlyUSD":4,"estimatedCostUSD":2,"credits":4,"marginUSD":2,"routeKey":"aws:linux:beast","available":true,"routeShare":0.75},"candidates":[{"provider":"aws","target":"linux","class":"beast","serverType":"beast","priority":10,"weight":3,"ttlSeconds":3600,"costHourlyUSD":2,"retailHourlyUSD":4,"estimatedCostUSD":2,"credits":4,"marginUSD":2,"routeKey":"aws:linux:beast","available":true,"routeShare":0.75},{"provider":"hetzner","target":"linux","class":"beast","serverType":"beast","priority":10,"weight":1,"ttlSeconds":3600,"costHourlyUSD":1,"retailHourlyUSD":2,"estimatedCostUSD":1,"credits":2,"marginUSD":1,"routeKey":"hetzner:linux:beast","available":true,"routeShare":0.25}],"warnings":["preview quote only"]},"marketplace":{"enabled":true,"mode":"preview","currency":"USD","creditUnit":"usd","requireCreditsForLeases":false,"supportedProviders":["aws","hetzner"],"features":{"quotes":true,"bidding":true,"payments":false,"creditLedger":false,"leaseEnforcement":false},"settlement":{"paymentProvider":"none","ledgerProvider":"none","providerSettlement":"external"},"decisionsRequired":["choose ledger"]}}`))
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "test-token")

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	err := app.Run(context.Background(), []string{
		"marketplace", "quote", "--class", "beast", "--strategy", "weighted",
	})
	if err != nil {
		t.Fatal(err)
	}
	if body.Strategy != "weighted" {
		t.Fatalf("strategy=%q", body.Strategy)
	}
	output := stdout.String()
	for _, want := range []string{
		"strategy=weighted",
		"selected provider=aws route=aws:linux:beast priority=10 weight=3.00 credits=$4.00",
		"share=75%",
		"share=25%",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestMarketplaceQuoteCommandRendersRoutingPlanLadder(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/marketplace/quotes" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		// active tier has three equal-weight routes (0.3334/0.3333/0.3333); largest-remainder display
		// must render 34%/33%/33% so the tier totals exactly 100%. A lower-priority failover tier follows.
		_, _ = w.Write([]byte(`{"quote":{"id":"mq_plan","mode":"preview","currency":"USD","creditUnit":"usd","strategy":"weighted","ttlSeconds":3600,"selected":{"provider":"aws","target":"linux","class":"beast","serverType":"beast","priority":10,"weight":1,"ttlSeconds":3600,"costHourlyUSD":1,"retailHourlyUSD":2,"estimatedCostUSD":1,"credits":2,"marginUSD":1,"routeKey":"aws:linux:beast","available":true,"routeShare":0.3334},"candidates":[],"routingPlan":[{"priority":10,"active":true,"members":[{"provider":"aws","routeKey":"aws:linux:beast","weight":1,"routeShare":0.3334},{"provider":"hetzner","routeKey":"hetzner:linux:beast","weight":1,"routeShare":0.3333},{"provider":"gcp","routeKey":"gcp:linux:beast","weight":1,"routeShare":0.3333}]},{"priority":5,"active":false,"members":[{"provider":"azure","routeKey":"azure:linux:beast","weight":1,"routeShare":1}]}],"warnings":["preview quote only"]},"marketplace":{"enabled":true,"mode":"preview","currency":"USD","creditUnit":"usd","requireCreditsForLeases":false,"supportedProviders":["aws","hetzner","gcp","azure"],"features":{"quotes":true,"bidding":true,"payments":false,"creditLedger":false,"leaseEnforcement":false},"settlement":{"paymentProvider":"none","ledgerProvider":"none","providerSettlement":"external"},"decisionsRequired":["choose ledger"]}}`))
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "test-token")

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run(context.Background(), []string{"marketplace", "quote", "--strategy", "weighted"}); err != nil {
		t.Fatal(err)
	}
	output := stdout.String()
	for _, want := range []string{
		"routing plan (failover order; preview only, no traffic routed):",
		"tier priority=10",
		"[active",
		"aws 34%",
		"hetzner 33%",
		"gcp 33%",
		"tier priority=5",
		"[failover",
		"azure 100%",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestParseMarketplaceTTL(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int64 // seconds; -1 means expect error
		wantErr bool
	}{
		{in: "30m", want: 1800},
		{in: "1h", want: 3600},
		{in: "3600s", want: 3600},
		{in: "3600", want: 3600}, // bare integer seconds
		{in: " 2h ", want: 7200}, // surrounding whitespace tolerated
		{in: "", wantErr: true},
		{in: "0", wantErr: true},
		{in: "-1h", wantErr: true},
		{in: "abc", wantErr: true},
	} {
		got, err := parseMarketplaceTTL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("parseMarketplaceTTL(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseMarketplaceTTL(%q) unexpected error: %v", tc.in, err)
		}
		if int64(got.Seconds()) != tc.want {
			t.Fatalf("parseMarketplaceTTL(%q) = %ds, want %ds", tc.in, int64(got.Seconds()), tc.want)
		}
	}
}

func TestMarketplaceRoutePercentsSumTo100PerTier(t *testing.T) {
	// three equal-weight routes: independent rounding of 33.33 would drift; largest-remainder must
	// hand the residual point to the first member so the tier renders 34/33/33 == 100.
	plan := []CoordinatorMarketplaceRouteTier{
		{
			Priority: 10,
			Active:   true,
			Members: []CoordinatorMarketplaceRouteTierMember{
				{Provider: "aws", RouteKey: "aws:linux:beast", RouteShare: 0.3334},
				{Provider: "hetzner", RouteKey: "hetzner:linux:beast", RouteShare: 0.3333},
				{Provider: "gcp", RouteKey: "gcp:linux:beast", RouteShare: 0.3333},
			},
		},
		{
			Priority: 5,
			Active:   false,
			Members: []CoordinatorMarketplaceRouteTierMember{
				{Provider: "azure", RouteKey: "azure:linux:beast", RouteShare: 1},
			},
		},
	}
	pct := marketplaceRoutePercents(plan)
	if pct["aws:linux:beast"] != 34 || pct["hetzner:linux:beast"] != 33 || pct["gcp:linux:beast"] != 33 {
		t.Fatalf("unexpected percents: %#v", pct)
	}
	for _, tier := range plan {
		sum := 0
		for _, member := range tier.Members {
			sum += pct[member.RouteKey]
		}
		if sum != 100 {
			t.Fatalf("tier priority=%d percents sum to %d, want 100 (%#v)", tier.Priority, sum, pct)
		}
	}
}

func TestMarketplaceShareSuffix(t *testing.T) {
	// with a routing plan present, the suffix uses the integer percents (which total 100 per tier)
	pct := map[string]int{"aws:linux:beast": 75}
	if got := marketplaceShareSuffix("aws:linux:beast", 0.75, pct); got != " share=75%" {
		t.Fatalf("planned share suffix = %q", got)
	}
	// no plan entry and a positive raw share -> fall back to rounding the 0..1 value
	if got := marketplaceShareSuffix("hetzner:linux:beast", 0.25, pct); got != " share=25%" {
		t.Fatalf("fallback share suffix = %q", got)
	}
	// no plan entry and no share -> empty (cheapest/balanced quotes render nothing)
	if got := marketplaceShareSuffix("gcp:linux:beast", 0, pct); got != "" {
		t.Fatalf("empty share suffix = %q", got)
	}
}
