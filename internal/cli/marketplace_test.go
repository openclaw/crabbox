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
}
