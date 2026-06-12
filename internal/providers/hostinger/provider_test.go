package hostinger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func isolateHostingerTestState(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
}

func TestProviderSpecAndFlags(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec=%#v", spec)
	}
	if len(Provider{}.Aliases()) != 0 {
		t.Fatalf("hostinger must not register aliases, got %#v", Provider{}.Aliases())
	}
	if !spec.Features.Has(core.FeatureSSH) || !spec.Features.Has(core.FeatureCrabboxSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("features=%#v; want ssh/crabbox-sync/cleanup", spec.Features)
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterHostingerProviderFlags(fs, core.Config{})
	for _, name := range []string{"hostinger-token", "hostinger-api-token", "hostinger-key"} {
		if fs.Lookup(name) != nil {
			t.Fatalf("hostinger API token surfaced as flag --%s", name)
		}
	}
	for _, name := range []string{"hostinger-url", "hostinger-item-id", "hostinger-payment-method-id", "hostinger-template-id", "hostinger-data-center-id", "hostinger-hostname-prefix", "hostinger-user", "hostinger-work-root", "hostinger-allow-purchase", "hostinger-release-action"} {
		if fs.Lookup(name) == nil {
			t.Fatalf("%s flag missing", name)
		}
	}
	if flag := fs.Lookup("hostinger-allow-purchase"); flag == nil || flag.DefValue != "false" {
		t.Fatalf("hostinger-allow-purchase default=%v, want false", flag)
	}
}

func TestClientValidationAndRedaction(t *testing.T) {
	cfg := core.Config{Hostinger: core.HostingerConfig{APIURL: "https://developers.hostinger.com"}}
	if _, err := newClient(cfg, core.Runtime{}); err == nil {
		t.Fatal("newClient accepted empty token")
	}
	cfg.Hostinger.APIToken = "secret-token"
	cfg.Hostinger.APIURL = "http://developers.hostinger.com"
	if _, err := newClient(cfg, core.Runtime{}); err == nil {
		t.Fatal("newClient accepted plaintext non-loopback url")
	}
	cfg.Hostinger.APIURL = "http://127.0.0.1:8080"
	if _, err := newClient(cfg, core.Runtime{}); err != nil {
		t.Fatalf("loopback http rejected: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		http.Error(w, `{"error":"secret-token is invalid"}`, http.StatusUnauthorized)
	}))
	defer server.Close()
	cfg.Hostinger.APIURL = server.URL
	client, err := newClient(cfg, core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListVMs(context.Background())
	if err == nil {
		t.Fatal("expected API error")
	}
	if strings.Contains(err.Error(), "secret-token") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("token was not redacted: %v", err)
	}
}

func TestClientRejectsRedirectBeforeForwardingToken(t *testing.T) {
	var targetAuthorization string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetAuthorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `[]`)
	}))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client, err := newClient(core.Config{Hostinger: core.HostingerConfig{
		APIToken: "secret-token",
		APIURL:   source.URL,
	}}, core.Runtime{HTTP: source.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListVMs(context.Background()); err == nil || !strings.Contains(err.Error(), "refused cross-origin or insecure redirect") {
		t.Fatalf("ListVMs err=%v", err)
	}
	if targetAuthorization != "" {
		t.Fatal("Hostinger token reached redirect target")
	}
}

func TestClientRoutesAndResponseLimits(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/billing/v1/catalog":
			if r.URL.Query().Get("category") != "VPS" {
				t.Fatalf("catalog category=%q", r.URL.Query().Get("category"))
			}
			_, _ = io.WriteString(w, `[{"id":"hostingercom-vps-kvm2","name":"KVM 2","category":"VPS","prices":[{"id":"hostingercom-vps-kvm2-usd-1m","currency":"USD","price":1799,"first_period_price":899,"period":1,"period_unit":"month"}]}]`)
		case "/api/billing/v1/payment-methods":
			_, _ = io.WriteString(w, `[{"id":7,"name":"Credit Card","payment_method":"card","is_default":true,"is_expired":false,"is_suspended":false}]`)
		case "/api/vps/v1/data-centers":
			_, _ = io.WriteString(w, `[{"id":1,"name":"EU"}]`)
		case "/api/vps/v1/templates":
			_, _ = io.WriteString(w, `[{"id":2,"name":"Ubuntu"}]`)
		case "/api/vps/v1/virtual-machines":
			if r.Method == http.MethodPost {
				var body struct {
					ItemID          string `json:"item_id"`
					PaymentMethodID int64  `json:"payment_method_id"`
					Setup           struct {
						TemplateID    int64  `json:"template_id"`
						DataCenterID  int64  `json:"data_center_id"`
						Hostname      string `json:"hostname"`
						EnableBackups *bool  `json:"enable_backups"`
						PublicKey     *struct {
							Name string `json:"name"`
							Key  string `json:"key"`
						} `json:"public_key"`
					} `json:"setup"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode purchase body: %v", err)
				}
				if body.ItemID != "hostingercom-vps-kvm2-usd-1m" || body.PaymentMethodID != 7 || body.Setup.TemplateID != 2 || body.Setup.DataCenterID != 3 || body.Setup.Hostname == "" {
					t.Fatalf("purchase body=%#v", body)
				}
				if body.Setup.EnableBackups == nil || *body.Setup.EnableBackups {
					t.Fatalf("enable_backups=%v; want explicit false", body.Setup.EnableBackups)
				}
				if body.Setup.PublicKey == nil || body.Setup.PublicKey.Name != "key" || body.Setup.PublicKey.Key != "ssh-ed25519 AAAA" {
					t.Fatalf("public_key=%#v", body.Setup.PublicKey)
				}
				_, _ = io.WriteString(w, `{"virtual_machine":{"id":4,"hostname":"crabbox-blue-abcdef123456","state":"running","ipv4":[{"address":"203.0.113.10"}]}}`)
			} else {
				_, _ = io.WriteString(w, `[]`)
			}
		case "/api/vps/v1/virtual-machines/4", "/api/vps/v1/virtual-machines/4/setup":
			_, _ = io.WriteString(w, `{"id":4,"hostname":"crabbox-blue-abcdef123456","state":"running","ipv4":[{"address":"203.0.113.10"}]}`)
		case "/api/vps/v1/virtual-machines/4/start", "/api/vps/v1/virtual-machines/4/stop":
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected route %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := newClient(core.Config{Hostinger: core.HostingerConfig{APIToken: "token", APIURL: server.URL}}, core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	catalog, err := client.ListCatalog(ctx)
	if err != nil || len(catalog) != 1 || len(catalog[0].Prices) != 1 {
		t.Fatalf("catalog=%#v err=%v", catalog, err)
	}
	if methods, err := client.ListPaymentMethods(ctx); err != nil || len(methods) != 1 || !methods[0].IsDefault {
		t.Fatalf("payment methods=%#v err=%v", methods, err)
	}
	if _, err := client.ListDataCenters(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListTemplates(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListVMs(ctx); err != nil {
		t.Fatal(err)
	}
	vm, err := client.PurchaseVM(ctx, hostingerPurchaseInput{
		ItemID:          "hostingercom-vps-kvm2-usd-1m",
		PaymentMethodID: 7,
		Setup: hostingerSetupInput{
			TemplateID:   2,
			DataCenterID: 3,
			Hostname:     "crabbox-blue-abcdef123456",
			PublicKey:    &hostingerSetupPublicKey{Name: "key", Key: "ssh-ed25519 AAAA"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if vm.IDString() != "4" || vm.Host() != "203.0.113.10" {
		t.Fatalf("vm=%#v", vm)
	}
	if _, err := client.SetupVM(ctx, "4", hostingerSetupInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetVM(ctx, "4"); err != nil {
		t.Fatal(err)
	}
	if err := client.StartVM(ctx, "4"); err != nil {
		t.Fatal(err)
	}
	if err := client.StopVM(ctx, "4"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 10 {
		t.Fatalf("routes=%v", seen)
	}

	oversized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), hostingerMaxResponseBytes+1))
	}))
	defer oversized.Close()
	client, err = newClient(core.Config{Hostinger: core.HostingerConfig{APIToken: "token", APIURL: oversized.URL}}, core.Runtime{HTTP: oversized.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListVMs(ctx); err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("oversized response err=%v", err)
	}
}

func TestAcquireRequiresExplicitPurchase(t *testing.T) {
	backend := NewLeaseBackend(Provider{}.Spec(), core.Config{Hostinger: core.HostingerConfig{APIToken: "token"}}, core.Runtime{}).(*leaseBackend)
	backend.client = &fakeAPI{}
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "allow-purchase") {
		t.Fatalf("Acquire err=%v", err)
	}
	if backend.client.(*fakeAPI).purchaseCalls != 0 {
		t.Fatal("Acquire called purchase while allow-purchase=false")
	}
}

func TestAcquirePreflightsLocalToolsBeforeAPI(t *testing.T) {
	api := &fakeAPI{}
	cfg := core.Config{Hostinger: core.HostingerConfig{
		APIToken:      "token",
		ItemID:        "hostingercom-vps-kvm2-usd-1m",
		TemplateID:    "2",
		DataCenterID:  "3",
		AllowPurchase: true,
	}}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{}).(*leaseBackend)
	backend.client = api

	oldLookPath := hostingerLookPath
	hostingerLookPath = func(tool string) (string, error) {
		if tool == "ssh" {
			return "", errors.New("missing")
		}
		return "/usr/bin/" + tool, nil
	}
	t.Cleanup(func() { hostingerLookPath = oldLookPath })

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "requires local ssh before billable") {
		t.Fatalf("Acquire err=%v", err)
	}
	if api.purchaseCalls != 0 || api.listCalls != 0 {
		t.Fatalf("preflight reached Hostinger API: purchase=%d list=%d", api.purchaseCalls, api.listCalls)
	}
}

func TestHostingerPurchaseAmbiguityClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "transport", err: errors.New("connection reset"), want: true},
		{name: "server", err: &hostingerAPIError{StatusCode: http.StatusBadGateway}, want: true},
		{name: "timeout", err: &hostingerAPIError{StatusCode: http.StatusRequestTimeout}, want: true},
		{name: "rate-limit", err: &hostingerAPIError{StatusCode: http.StatusTooManyRequests}, want: false},
		{name: "payment", err: &hostingerAPIError{StatusCode: http.StatusPaymentRequired}, want: false},
		{name: "validation", err: &hostingerAPIError{StatusCode: http.StatusUnprocessableEntity}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostingerPurchaseMayHaveSucceeded(tc.err); got != tc.want {
				t.Fatalf("hostingerPurchaseMayHaveSucceeded(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestAcquireRemovesRecoveryStateAfterDefinitivePurchaseFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		status     string
	}{
		{name: "payment-required", statusCode: http.StatusPaymentRequired, status: "402 Payment Required"},
		{name: "rate-limited", statusCode: http.StatusTooManyRequests, status: "429 Too Many Requests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHostingerTestState(t)
			api := &fakeAPI{
				purchaseErrBeforeCreate: &hostingerAPIError{
					StatusCode: tc.statusCode,
					Status:     tc.status,
				},
			}
			cfg := core.Config{Hostinger: core.HostingerConfig{
				APIToken:       "token",
				ItemID:         "hostingercom-vps-kvm2-usd-1m",
				TemplateID:     "2",
				DataCenterID:   "3",
				HostnamePrefix: "crabbox",
				AllowPurchase:  true,
			}}
			backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
			backend.client = api

			_, err := backend.Acquire(context.Background(), core.AcquireRequest{
				Repo:          core.Repo{Root: t.TempDir()},
				RequestedSlug: "rejected",
			})
			if err == nil || !strings.Contains(err.Error(), tc.status) {
				t.Fatalf("Acquire err=%v", err)
			}
			claims, listErr := core.ListLeaseClaims()
			if listErr != nil || len(claims) != 0 {
				t.Fatalf("claims=%#v err=%v", claims, listErr)
			}
			configDir, configErr := os.UserConfigDir()
			if configErr != nil {
				t.Fatal(configErr)
			}
			leaseDirs, globErr := filepath.Glob(filepath.Join(configDir, "crabbox", "testboxes", "*"))
			if globErr != nil {
				t.Fatal(globErr)
			}
			if len(leaseDirs) != 0 {
				t.Fatalf("definitive purchase failure retained recovery state: %v", leaseDirs)
			}
		})
	}
}

func TestAcquireRejectsUnsupportedTemplateBeforePurchase(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{
		templates: []hostingerTemplate{{ID: "2", Name: "AlmaLinux 9"}},
	}
	cfg := core.Config{Hostinger: core.HostingerConfig{
		APIToken:      "token",
		ItemID:        "hostingercom-vps-kvm2-usd-1m",
		TemplateID:    "2",
		DataCenterID:  "3",
		AllowPurchase: true,
	}}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{}).(*leaseBackend)
	backend.client = api

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "choose an Ubuntu or Debian template") {
		t.Fatalf("Acquire err=%v", err)
	}
	if api.purchaseCalls != 0 || api.listCalls != 0 {
		t.Fatalf("unsupported template reached VPS API: purchase=%d list=%d", api.purchaseCalls, api.listCalls)
	}
}

func TestAcquireRejectsNonNumericHostingerSetupIDs(t *testing.T) {
	api := &fakeAPI{}
	cfg := core.Config{Hostinger: core.HostingerConfig{
		APIToken:      "token",
		ItemID:        "hostingercom-vps-kvm2-usd-1m",
		TemplateID:    "tpl-ubuntu",
		DataCenterID:  "3",
		AllowPurchase: true,
	}}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{}).(*leaseBackend)
	backend.client = api

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "numeric hostinger template id") {
		t.Fatalf("Acquire err=%v", err)
	}
	if api.purchaseCalls != 0 {
		t.Fatal("Acquire called purchase with non-numeric Hostinger IDs")
	}
}

func TestAcquireRejectsInvalidGeneratedHostnameBeforePurchase(t *testing.T) {
	for _, prefix := range []string{"bad_prefix", "bad prefix", strings.Repeat("a", 60)} {
		t.Run(prefix, func(t *testing.T) {
			isolateHostingerTestState(t)
			api := &fakeAPI{}
			cfg := core.Config{Hostinger: core.HostingerConfig{
				APIToken:       "token",
				ItemID:         "hostingercom-vps-kvm2-usd-1m",
				TemplateID:     "2",
				DataCenterID:   "3",
				HostnamePrefix: prefix,
				AllowPurchase:  true,
			}}
			backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
			backend.client = api

			_, err := backend.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "test"})
			if err == nil || !strings.Contains(err.Error(), "generated hostname") {
				t.Fatalf("Acquire err=%v", err)
			}
			if api.purchaseCalls != 0 {
				t.Fatalf("invalid hostname reached purchase: %#v", api.purchase)
			}
		})
	}
}

func TestAcquireRequiresUsableDefaultPaymentMethod(t *testing.T) {
	api := &fakeAPI{paymentMethods: []hostingerPaymentMethod{}}
	cfg := core.Config{Hostinger: core.HostingerConfig{
		APIToken:      "token",
		ItemID:        "hostingercom-vps-kvm2-usd-1m",
		TemplateID:    "2",
		DataCenterID:  "3",
		AllowPurchase: true,
	}}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{}).(*leaseBackend)
	backend.client = api

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "active default Hostinger payment method") {
		t.Fatalf("Acquire err=%v", err)
	}
	if api.purchaseCalls != 0 {
		t.Fatal("Acquire called purchase without a usable payment method")
	}
}

func TestDoctorFailsInvalidPurchaseOptions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*core.Config, *fakeAPI)
		want   string
	}{
		{
			name: "priced-item",
			mutate: func(cfg *core.Config, _ *fakeAPI) {
				cfg.Hostinger.ItemID = "missing-item"
			},
			want: "not a current priced VPS item",
		},
		{
			name: "payment-method",
			mutate: func(_ *core.Config, api *fakeAPI) {
				api.paymentMethods = []hostingerPaymentMethod{}
			},
			want: "active default Hostinger payment method",
		},
		{
			name: "template",
			mutate: func(_ *core.Config, api *fakeAPI) {
				api.templates = []hostingerTemplate{{ID: "2", Name: "AlmaLinux 9"}}
			},
			want: "choose an Ubuntu or Debian template",
		},
		{
			name: "data-center",
			mutate: func(cfg *core.Config, _ *fakeAPI) {
				cfg.Hostinger.DataCenterID = "99"
			},
			want: "data center id",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.Config{Hostinger: core.HostingerConfig{
				APIToken:      "token",
				ItemID:        "hostingercom-vps-kvm2-usd-1m",
				TemplateID:    "2",
				DataCenterID:  "3",
				AllowPurchase: true,
			}}
			api := &fakeAPI{}
			tc.mutate(&cfg, api)
			backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{}).(*leaseBackend)
			backend.client = api

			result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Checks) != 2 ||
				result.Checks[1].Status != "failed" ||
				!strings.Contains(result.Checks[1].Message, tc.want) {
				t.Fatalf("doctor checks=%#v", result.Checks)
			}
		})
	}
}

func TestDoctorDiscoversPurchaseOptionsBeforeSelectorsAreConfigured(t *testing.T) {
	api := &fakeAPI{}
	backend := NewLeaseBackend(Provider{}.Spec(), core.Config{
		Hostinger: core.HostingerConfig{APIToken: "token"},
	}, core.Runtime{}).(*leaseBackend)
	backend.client = api

	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Checks) != 2 ||
		result.Checks[1].Status != "warning" ||
		!strings.Contains(result.Checks[1].Message, "configuration=incomplete") ||
		result.Checks[1].Details["priced_items"] == "" ||
		result.Checks[1].Details["templates"] == "" ||
		result.Checks[1].Details["data_centers"] == "" {
		t.Fatalf("doctor checks=%#v", result.Checks)
	}
	if api.purchaseCalls != 0 || api.startCalls != 0 || api.stopCalls != 0 {
		t.Fatalf("doctor mutated Hostinger resources: %#v", api)
	}
}

func TestAcquireRejectsUnsupportedReleaseActionBeforePurchase(t *testing.T) {
	api := &fakeAPI{}
	cfg := core.Config{Hostinger: core.HostingerConfig{
		APIToken:      "token",
		ItemID:        "hostingercom-vps-kvm2-usd-1m",
		TemplateID:    "2",
		DataCenterID:  "3",
		AllowPurchase: true,
		ReleaseAction: "delete",
	}}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{}).(*leaseBackend)
	backend.client = api

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "release action must be stop") {
		t.Fatalf("Acquire err=%v", err)
	}
	if api.purchaseCalls != 0 {
		t.Fatal("Acquire called purchase with unsupported release action")
	}
}

func TestHostingerBootstrapScriptInstallsReadyMarker(t *testing.T) {
	cfg := core.Config{SSHUser: "root", WorkRoot: "/work/crabbox"}
	script := hostingerBootstrapScript(cfg)
	for _, want := range []string{
		"have_crabbox_tools()",
		"apt-get install -y --no-install-recommends openssh-server ca-certificates curl git rsync jq",
		"safe_work_root_chown=0",
		`canonical_work_root=$(readlink -m -- "$work_root")`,
		`chown -h -- "$user:$group" "$work_root"`,
		"tee /usr/local/bin/crabbox-ready >/dev/null <<'READY'",
		"test -w '/work/crabbox'",
		"touch /var/lib/crabbox/bootstrapped",
		"test -w \"$work_root\"",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("bootstrap script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, `chown -R "$user:$group" "$work_root"`) {
		t.Fatalf("bootstrap script recursively chowns configured work root:\n%s", script)
	}
}

func TestValidateHostingerWorkRoot(t *testing.T) {
	for _, root := range []string{
		"/work/crabbox",
		"/work/crabbox/project",
		"/workspaces/crabbox",
		"/var/lib/crabbox/work/project",
		"/opt/crabbox/project",
		"/home/ubuntu/crabbox/project",
	} {
		if err := validateHostingerWorkRoot(core.Config{WorkRoot: root, SSHUser: "ubuntu"}); err != nil {
			t.Fatalf("valid root %q: %v", root, err)
		}
	}
	for _, root := range []string{
		"",
		" /work/crabbox",
		"/work/crabbox ",
		"work/crabbox",
		"/work/crabbox/../../etc",
		"/work/crabbox/../other",
		"/home/other/crabbox",
		"/etc/crabbox",
	} {
		if err := validateHostingerWorkRoot(core.Config{WorkRoot: root, SSHUser: "ubuntu"}); err == nil {
			t.Fatalf("invalid root accepted: %q", root)
		}
	}
	for _, user := range []string{"", " ubuntu", "ubuntu ", "-oProxyCommand=touch /tmp/pwned", "user@host", "user/name", strings.Repeat("a", 33)} {
		if err := validateHostingerWorkRoot(core.Config{WorkRoot: "/home/ubuntu/crabbox", SSHUser: user}); err == nil {
			t.Fatalf("invalid SSH user accepted: %q", user)
		}
	}
}

func TestHostingerReadyCheckUsesConfiguredWorkRoot(t *testing.T) {
	cfg := core.Config{WorkRoot: "/home/runner/crabbox"}
	got := hostingerReadyCheck(cfg)
	for _, want := range []string{"git --version", "rsync --version", "curl --version", "jq --version", "test -w '/home/runner/crabbox'"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ready check missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "/usr/local/bin/crabbox-ready") {
		t.Fatalf("ready check should not require privileged helper: %s", got)
	}
	if strings.Contains(got, "/tmp/") {
		t.Fatalf("ready check uses predictable temporary path: %s", got)
	}
}

func TestHostingerResolveBootstrapsOnlyWhenPrepareRequested(t *testing.T) {
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-existing", Hostname: "srv.example.test", State: "running", IPv4: hostingerIPAddresses{"203.0.113.50"}}},
	}
	cfg := core.Config{
		SSHKey: "/tmp/test-key",
		Hostinger: core.HostingerConfig{
			APIToken: "token",
		},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	oldRunSSHQuiet := hostingerRunSSHQuiet
	calls := 0
	hostingerRunSSHQuiet = func(_ context.Context, _ SSHTarget, remote string) error {
		calls++
		if !strings.Contains(remote, "crabbox-ready") {
			t.Fatalf("bootstrap command missing ready helper: %s", remote)
		}
		return nil
	}
	t.Cleanup(func() { hostingerRunSSHQuiet = oldRunSSHQuiet })

	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "vm-existing"}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("read-only resolve bootstrap calls=%d, want 0", calls)
	}

	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "vm-existing", Prepare: true}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("prepared resolve bootstrap calls=%d, want 1", calls)
	}
}

func TestHostingerAcquireBootstrapsBeforeSSHReady(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{}
	cfg := core.Config{Hostinger: core.HostingerConfig{
		APIToken:       "token",
		ItemID:         "hostingercom-vps-kvm2-usd-1m",
		TemplateID:     "2",
		DataCenterID:   "3",
		HostnamePrefix: "crabbox",
		AllowPurchase:  true,
	}}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	oldRunSSHQuiet := hostingerRunSSHQuiet
	oldWaitForSSHReady := hostingerWaitForSSHReady
	var calls []string
	hostingerRunSSHQuiet = func(_ context.Context, _ SSHTarget, _ string) error {
		calls = append(calls, "bootstrap")
		return nil
	}
	hostingerWaitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		calls = append(calls, "ready")
		return nil
	}
	t.Cleanup(func() {
		hostingerRunSSHQuiet = oldRunSSHQuiet
		hostingerWaitForSSHReady = oldWaitForSSHReady
	})

	if _, err := backend.Acquire(context.Background(), core.AcquireRequest{Keep: true, RequestedSlug: "blue"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "bootstrap,ready" {
		t.Fatalf("bootstrap/ready calls=%v, want bootstrap before ready", calls)
	}
}

func TestAcquireResolveListReleaseCleanupDoctorWithFakeAPI(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-existing", Hostname: "crabbox-green-abcdef123456", State: "running", IPv4: hostingerIPAddresses{"203.0.113.20"}}},
	}
	var stderr bytes.Buffer
	cfg := core.Config{Hostinger: core.HostingerConfig{
		APIToken:       "token",
		ItemID:         "hostingercom-vps-kvm2-usd-1m",
		TemplateID:     "2",
		DataCenterID:   "3",
		HostnamePrefix: "crabbox",
		AllowPurchase:  true,
	}}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: &stderr}).(*leaseBackend)
	backend.client = api
	backend.skipSSHWait = true

	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Keep: true, RequestedSlug: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if api.purchaseCalls != 1 || api.setupCalls != 0 || lease.SSH.Host != "203.0.113.42" || lease.SSH.Port != "22" {
		t.Fatalf("lease=%#v api=%#v", lease, api)
	}
	if !strings.Contains(api.purchase.Setup.Hostname, "crabbox-blue-") {
		t.Fatalf("purchase payload=%#v", api.purchase)
	}
	if api.purchase.Setup.EnableBackups {
		t.Fatalf("purchase payload enabled backups: %#v", api.purchase)
	}
	if api.purchase.Setup.PublicKey == nil || api.purchase.Setup.PublicKey.Name == "" || !strings.HasPrefix(api.purchase.Setup.PublicKey.Key, "ssh-") {
		t.Fatalf("purchase payload missing setup-time public key: %#v", api.purchase)
	}
	if api.purchase.PaymentMethodID != 7 {
		t.Fatalf("purchase payment method=%d, want default id 7", api.purchase.PaymentMethodID)
	}

	list, err := backend.List(context.Background(), core.ListRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list len=%d", len(list))
	}
	api.vms = append(api.vms, hostingerVM{ID: "vm-manual", Hostname: "manual-vm", State: "running", IPv4: hostingerIPAddresses{"203.0.113.99"}})
	ownedOnly, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ownedOnly) != 1 {
		t.Fatalf("owned list len=%d, want 1", len(ownedOnly))
	}
	all, err := backend.List(context.Background(), core.ListRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("all list len=%d, want manual inventory included", len(all))
	}
	resolved, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "vm-new"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Server.CloudID != "vm-new" {
		t.Fatalf("resolved=%#v", resolved)
	}
	if resolved.SSH.Key == "" {
		t.Fatalf("resolved SSH key is empty")
	}
	if resolved.SSH.ReadyCheck == "" || !strings.Contains(resolved.SSH.ReadyCheck, "jq --version") || !strings.Contains(resolved.SSH.ReadyCheck, "test -w") {
		t.Fatalf("resolved ready check is incomplete: %q", resolved.SSH.ReadyCheck)
	}
	if _, err := backend.Touch(context.Background(), core.TouchRequest{Lease: resolved, State: "busy"}); err != nil {
		t.Fatal(err)
	}
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || !strings.Contains(result.Message, "purchase=explicit") {
		t.Fatalf("doctor=%#v", result)
	}
	if len(result.Checks) != 2 ||
		result.Checks[0].Check != "provider" ||
		!strings.Contains(result.Checks[0].Message, "mutation=false") ||
		result.Checks[1].Check != "purchase-options" ||
		!strings.Contains(result.Checks[1].Details["priced_items"], "hostingercom-vps-kvm2-usd-1m") ||
		!strings.Contains(result.Checks[1].Details["payment_methods"], "7=Credit Card(active+default)") {
		t.Fatalf("doctor discovery=%#v", result.Checks)
	}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if api.stopCalls != 0 {
		t.Fatal("dry-run cleanup stopped a VM")
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatal(err)
	}
	if api.stopCalls != 1 || api.stopped[0] != "vm-new" {
		t.Fatalf("stops=%v", api.stopped)
	}
	if msg := backend.ReleaseLeaseMessage(resolved); !strings.Contains(msg, "stopped") || !strings.Contains(msg, "billing=still-owned") {
		t.Fatalf("release message=%q", msg)
	}
	if _, err := os.Stat(resolved.SSH.Key); err != nil {
		t.Fatalf("release removed reusable SSH key %s: %v", resolved.SSH.Key, err)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(resolved.LeaseID, providerName)
	if err != nil || !ok || claim.CloudID != "vm-new" || claim.Labels["state"] != "stopped" {
		t.Fatalf("stopped claim=%#v ok=%v err=%v", claim, ok, err)
	}
	if claim.SSHHost != "" || claim.SSHPort != 0 {
		t.Fatalf("stopped claim endpoint=%s:%d want cleared", claim.SSHHost, claim.SSHPort)
	}
	restarted, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID:   resolved.LeaseID,
		Repo: core.Repo{Root: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if api.startCalls != 1 || restarted.SSH.Key != resolved.SSH.Key || restarted.Server.CloudID != "vm-new" {
		t.Fatalf("restarted=%#v starts=%v", restarted, api.started)
	}
}

func TestResolveVMUsesDirectHostingerGetForVMID(t *testing.T) {
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-direct", Hostname: "manual-vm", State: "running", IPv4: hostingerIPAddresses{"203.0.113.88"}}},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), core.Config{Hostinger: core.HostingerConfig{APIToken: "token"}}, core.Runtime{}).(*leaseBackend)

	vm, _, _, err := backend.resolveVM(context.Background(), api, "vm-direct")
	if err != nil {
		t.Fatal(err)
	}
	if vm.IDString() != "vm-direct" {
		t.Fatalf("vm=%#v", vm)
	}
	if api.getCalls != 1 || api.listCalls != 0 {
		t.Fatalf("getCalls=%d listCalls=%d, want direct get only", api.getCalls, api.listCalls)
	}
}

func TestWaitForVMRequiresPublicIPBeforeReady(t *testing.T) {
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-new", Hostname: "srv.example.test", State: "running"}},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), core.Config{Hostinger: core.HostingerConfig{APIToken: "token"}}, core.Runtime{}).(*leaseBackend)

	oldSleep := hostingerSleep
	hostingerSleep = func(time.Duration) {
		api.vms[0].IPv4 = hostingerIPAddresses{"203.0.113.77"}
	}
	t.Cleanup(func() { hostingerSleep = oldSleep })

	vm, err := backend.waitForVM(context.Background(), api, "vm-new")
	if err != nil {
		t.Fatal(err)
	}
	if api.getCalls < 2 {
		t.Fatalf("getCalls=%d, want wait for second poll with public IP", api.getCalls)
	}
	if vm.Host() != "203.0.113.77" {
		t.Fatalf("host=%q, want public IP before ready", vm.Host())
	}
}

func TestWaitForVMFailsFastOnTerminalState(t *testing.T) {
	for _, state := range []string{"error", "suspended", "destroyed"} {
		t.Run(state, func(t *testing.T) {
			api := &fakeAPI{
				vms: []hostingerVM{{ID: "vm-terminal", Hostname: "srv.example.test", State: state}},
			}
			backend := NewLeaseBackend(Provider{}.Spec(), core.Config{Hostinger: core.HostingerConfig{APIToken: "token"}}, core.Runtime{}).(*leaseBackend)
			oldSleep := hostingerSleep
			hostingerSleep = func(time.Duration) { t.Fatal("terminal state should not sleep") }
			t.Cleanup(func() { hostingerSleep = oldSleep })

			_, err := backend.waitForVM(context.Background(), api, "vm-terminal")
			if err == nil || !strings.Contains(err.Error(), "terminal state="+state) {
				t.Fatalf("waitForVM error=%v", err)
			}
			if api.getCalls != 1 {
				t.Fatalf("getCalls=%d, want one", api.getCalls)
			}
		})
	}
}

func TestAcquireRecoversAmbiguousPurchaseByExactHostname(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{purchaseErrAfterCreate: errors.New("request timed out")}
	cfg := core.Config{Hostinger: core.HostingerConfig{
		APIToken:       "token",
		ItemID:         "hostingercom-vps-kvm2-usd-1m",
		TemplateID:     "2",
		DataCenterID:   "3",
		HostnamePrefix: "crabbox",
		AllowPurchase:  true,
	}}
	var stderr bytes.Buffer
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: &stderr}).(*leaseBackend)
	backend.client = api
	backend.skipSSHWait = true
	repoRoot := t.TempDir()

	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: repoRoot}, Keep: true, RequestedSlug: "recover"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "vm-new" || api.purchaseCalls != 1 || api.listCalls < 2 {
		t.Fatalf("lease=%#v api=%#v", lease, api)
	}
	if !strings.Contains(stderr.String(), "recovered ambiguous hostinger purchase") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(lease.LeaseID, providerName)
	if err != nil || !ok || claim.CloudID != "vm-new" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestAcquireRetainsPendingClaimForInvisibleAmbiguousPurchase(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{purchaseErrBeforeCreate: errors.New("request timed out")}
	cfg := core.Config{
		Provider: providerName,
		Hostinger: core.HostingerConfig{
			APIToken:       "token",
			ItemID:         "hostingercom-vps-kvm2-usd-1m",
			TemplateID:     "2",
			DataCenterID:   "3",
			HostnamePrefix: "crabbox",
			AllowPurchase:  true,
		},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api
	backend.skipSSHWait = true

	oldTimeout := hostingerPurchaseRecoveryTimeout
	hostingerPurchaseRecoveryTimeout = time.Nanosecond
	t.Cleanup(func() { hostingerPurchaseRecoveryTimeout = oldTimeout })

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		Keep:          true,
		RequestedSlug: "pending",
	})
	if err == nil || !strings.Contains(err.Error(), "recovery claim retained") {
		t.Fatalf("Acquire err=%v", err)
	}
	if api.stopCalls != 0 {
		t.Fatalf("ambiguous purchase without a vm id attempted stop: %v", api.stopped)
	}

	claims, listErr := core.ListLeaseClaims()
	if listErr != nil || len(claims) != 1 {
		t.Fatalf("claims=%#v err=%v", claims, listErr)
	}
	claim := claims[0]
	hostname := claim.Labels[hostingerRecoveryHostnameLabel]
	if claim.CloudID != "" ||
		claim.Labels[hostingerRecoveryLabel] != hostingerRecoveryAmbiguous ||
		!strings.HasPrefix(hostname, "crabbox-pending-") {
		t.Fatalf("pending claim=%#v", claim)
	}
	keyPath, keyErr := core.TestboxKeyPath(claim.LeaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("pending recovery key missing: %v", statErr)
	}

	api.vms = append(api.vms, hostingerVM{
		ID:       "vm-late",
		Hostname: hostname,
		State:    "running",
		IPv4:     hostingerIPAddresses{"203.0.113.43"},
	})
	lease, resolveErr := backend.Resolve(context.Background(), core.ResolveRequest{ID: claim.LeaseID})
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if lease.LeaseID != claim.LeaseID || lease.Server.CloudID != "vm-late" {
		t.Fatalf("lease=%#v", lease)
	}
	recovered, ok, resolveClaimErr := core.ResolveLeaseClaimForProvider(claim.LeaseID, providerName)
	if resolveClaimErr != nil || !ok {
		t.Fatalf("claim=%#v ok=%v err=%v", recovered, ok, resolveClaimErr)
	}
	if recovered.CloudID != "vm-late" ||
		recovered.Labels[hostingerRecoveryLabel] != "" ||
		recovered.Labels[hostingerRecoveryHostnameLabel] != "" {
		t.Fatalf("recovered claim=%#v", recovered)
	}
}

func TestAcquireRecoversAfterPendingClaimWriteFailure(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{purchaseErrAfterCreate: errors.New("request timed out")}
	cfg := core.Config{
		Provider: providerName,
		Hostinger: core.HostingerConfig{
			APIToken:       "token",
			ItemID:         "hostingercom-vps-kvm2-usd-1m",
			TemplateID:     "2",
			DataCenterID:   "3",
			HostnamePrefix: "crabbox",
			AllowPurchase:  true,
		},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api
	backend.skipSSHWait = true

	oldClaim := claimLeaseTargetForRepoConfigIfUnchanged
	claimCalls := 0
	claimLeaseTargetForRepoConfigIfUnchanged = func(leaseID, slug string, cfg Config, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
		claimCalls++
		if claimCalls == 1 {
			return LeaseClaim{}, errors.New("claim storage unavailable")
		}
		return oldClaim(leaseID, slug, cfg, server, target, repoRoot, idleTimeout, reclaim, expected, expectedExists)
	}
	t.Cleanup(func() { claimLeaseTargetForRepoConfigIfUnchanged = oldClaim })

	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		Keep:          true,
		RequestedSlug: "recover",
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimCalls != 2 || lease.Server.CloudID != "vm-new" {
		t.Fatalf("claimCalls=%d lease=%#v", claimCalls, lease)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider(lease.LeaseID, providerName)
	if claimErr != nil || !ok || claim.CloudID != "vm-new" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
}

func TestAcquireClaimFailureRetainsRecoverableLeaseKeyMapping(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{}
	cfg := core.Config{
		Provider: providerName,
		Hostinger: core.HostingerConfig{
			APIToken:       "token",
			ItemID:         "hostingercom-vps-kvm2-usd-1m",
			TemplateID:     "2",
			DataCenterID:   "3",
			HostnamePrefix: "crabbox",
			AllowPurchase:  true,
		},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api
	backend.skipSSHWait = true

	oldClaim := claimLeaseTargetForRepoConfigIfUnchanged
	claimLeaseTargetForRepoConfigIfUnchanged = func(string, string, Config, Server, SSHTarget, string, time.Duration, bool, LeaseClaim, bool) (LeaseClaim, error) {
		return LeaseClaim{}, errors.New("claim storage unavailable")
	}
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "recover",
	})
	claimLeaseTargetForRepoConfigIfUnchanged = oldClaim
	t.Cleanup(func() { claimLeaseTargetForRepoConfigIfUnchanged = oldClaim })
	if err == nil || !strings.Contains(err.Error(), "persist hostinger paid VPS claim") {
		t.Fatalf("Acquire err=%v", err)
	}
	if api.stopCalls != 1 || len(api.vms) != 1 || !api.vms[0].Stopped() {
		t.Fatalf("stops=%v vms=%#v", api.stopped, api.vms)
	}

	recovered, resolveErr := backend.Resolve(context.Background(), core.ResolveRequest{
		ID:      "vm-new",
		Repo:    core.Repo{Root: t.TempDir()},
		Reclaim: true,
	})
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if recovered.LeaseID == "cbx_hostinger_vm-new" || recovered.SSH.Key == "" || recovered.Server.CloudID != "vm-new" {
		t.Fatalf("recovered=%#v", recovered)
	}
	if _, ok, claimErr := core.ResolveLeaseClaimForProvider(recovered.LeaseID, providerName); claimErr != nil || !ok {
		t.Fatalf("recovered claim ok=%v err=%v", ok, claimErr)
	}
}

func TestRecoveryLookupSkipsCorruptUnrelatedRecords(t *testing.T) {
	isolateHostingerTestState(t)
	badPath, err := hostingerRecoveryRecordPath("cbx_badrecord")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(badPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := hostingerRecoveryRecord{
		LeaseID:  "cbx_goodrecord",
		Slug:     "good",
		VMID:     "vm-good",
		Hostname: "crabbox-good-goodrecord",
	}
	goodPath, err := hostingerRecoveryRecordPath(good.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(goodPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeHostingerRecoveryRecord(good); err != nil {
		t.Fatal(err)
	}

	record, ok, err := findHostingerRecoveryRecord(hostingerVM{ID: "vm-good", Hostname: good.Hostname})
	if err != nil || !ok || record.LeaseID != good.LeaseID {
		t.Fatalf("record=%#v ok=%v err=%v", record, ok, err)
	}
	if _, ok, err := findHostingerRecoveryRecord(hostingerVM{ID: "vm-other", Hostname: "other"}); err != nil || ok {
		t.Fatalf("unrelated lookup ok=%v err=%v", ok, err)
	}
}

func TestAcquireRetainsKeyWhenPendingClaimAndRecoveryFail(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{purchaseErrBeforeCreate: errors.New("request timed out")}
	cfg := core.Config{
		Provider: providerName,
		Hostinger: core.HostingerConfig{
			APIToken:       "token",
			ItemID:         "hostingercom-vps-kvm2-usd-1m",
			TemplateID:     "2",
			DataCenterID:   "3",
			HostnamePrefix: "crabbox",
			AllowPurchase:  true,
		},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	oldClaim := claimLeaseTargetForRepoConfigIfUnchanged
	claimLeaseTargetForRepoConfigIfUnchanged = func(string, string, Config, Server, SSHTarget, string, time.Duration, bool, LeaseClaim, bool) (LeaseClaim, error) {
		return LeaseClaim{}, errors.New("claim storage unavailable")
	}
	t.Cleanup(func() { claimLeaseTargetForRepoConfigIfUnchanged = oldClaim })
	oldTimeout := hostingerPurchaseRecoveryTimeout
	hostingerPurchaseRecoveryTimeout = time.Nanosecond
	t.Cleanup(func() { hostingerPurchaseRecoveryTimeout = oldTimeout })

	_, err := backend.Acquire(context.Background(), core.AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		Keep:          true,
		RequestedSlug: "pending",
	})
	if err == nil || !strings.Contains(err.Error(), "recovery claim failed and key retained") {
		t.Fatalf("Acquire err=%v", err)
	}
	message := err.Error()
	keyStart := strings.Index(message, " key=")
	keyEnd := strings.Index(message, ": purchase_error=")
	if keyStart < 0 || keyEnd <= keyStart {
		t.Fatalf("Acquire error missing retained key path: %v", err)
	}
	keyPath := message[keyStart+len(" key=") : keyEnd]
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("retained recovery key missing: %v", statErr)
	}
}

func TestAcquireFailureStopsPaidVPSButRetainsRecoveryState(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{}
	cfg := core.Config{Hostinger: core.HostingerConfig{
		APIToken:       "token",
		ItemID:         "hostingercom-vps-kvm2-usd-1m",
		TemplateID:     "2",
		DataCenterID:   "3",
		HostnamePrefix: "crabbox",
		AllowPurchase:  true,
	}}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api
	ctx, cancel := context.WithCancel(context.Background())

	oldRunSSHQuiet := hostingerRunSSHQuiet
	oldSleep := hostingerSleep
	hostingerRunSSHQuiet = func(context.Context, SSHTarget, string) error {
		return errors.New("ssh unavailable")
	}
	hostingerSleep = func(time.Duration) { cancel() }
	t.Cleanup(func() {
		hostingerRunSSHQuiet = oldRunSSHQuiet
		hostingerSleep = oldSleep
	})

	_, err := backend.Acquire(ctx, core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "failed"})
	if err == nil || !strings.Contains(err.Error(), "billing=still-owned") || !strings.Contains(err.Error(), "rollback=stopped") {
		t.Fatalf("Acquire err=%v", err)
	}
	if api.stopCalls != 1 || api.stopped[0] != "vm-new" {
		t.Fatalf("stops=%v", api.stopped)
	}
	var claim core.LeaseClaim
	claims, listErr := core.ListLeaseClaims()
	if listErr != nil || len(claims) != 1 {
		t.Fatalf("claims=%#v err=%v", claims, listErr)
	}
	claim = claims[0]
	if claim.CloudID != "vm-new" {
		t.Fatalf("claim=%#v", claim)
	}
	if claim.Labels["state"] != "stopped" || claim.SSHHost != "" || claim.SSHPort != 0 {
		t.Fatalf("rollback claim=%#v", claim)
	}
	keyPath, keyErr := core.TestboxKeyPath(claim.LeaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("recovery key missing: %v", statErr)
	}
}

func TestAcquireFailureDoesNotStopPaidVPSWhenClaimDisappears(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{}
	cfg := core.Config{Hostinger: core.HostingerConfig{
		APIToken:       "token",
		ItemID:         "hostingercom-vps-kvm2-usd-1m",
		TemplateID:     "2",
		DataCenterID:   "3",
		HostnamePrefix: "crabbox",
		AllowPurchase:  true,
	}}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api
	ctx, cancel := context.WithCancel(context.Background())

	oldRunSSHQuiet := hostingerRunSSHQuiet
	oldSleep := hostingerSleep
	hostingerRunSSHQuiet = func(context.Context, SSHTarget, string) error {
		claims, err := core.ListLeaseClaims()
		if err != nil {
			return err
		}
		for _, claim := range claims {
			core.RemoveLeaseClaim(claim.LeaseID)
		}
		return errors.New("ssh unavailable")
	}
	hostingerSleep = func(time.Duration) { cancel() }
	t.Cleanup(func() {
		hostingerRunSSHQuiet = oldRunSSHQuiet
		hostingerSleep = oldSleep
	})

	_, err := backend.Acquire(ctx, core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "missing-claim"})
	if err == nil || !strings.Contains(err.Error(), "stop-skipped") || !strings.Contains(err.Error(), "claim changed; retry") {
		t.Fatalf("Acquire err=%v", err)
	}
	if api.stopCalls != 0 {
		t.Fatalf("stops=%v", api.stopped)
	}
}

func TestResolveStartsStoppedHostingerVMForSSHCommands(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-stopped", Hostname: "crabbox-blue-abcdef123456", State: "stopped"}},
	}
	cfg := core.Config{
		Provider: providerName,
		SSHKey:   "/tmp/test-key",
		Hostinger: core.HostingerConfig{
			APIToken: "token",
		},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api
	oldWaitForSSHReady := hostingerWaitForSSHReady
	oldRunSSHQuiet := hostingerRunSSHQuiet
	waitCalls := 0
	hostingerWaitForSSHReady = func(_ context.Context, target *SSHTarget, _ io.Writer, phase string, _ time.Duration) error {
		waitCalls++
		if phase != "restart" || target.ReadyCheck != "true" {
			t.Fatalf("restart wait phase=%q readyCheck=%q", phase, target.ReadyCheck)
		}
		return nil
	}
	hostingerRunSSHQuiet = func(context.Context, SSHTarget, string) error { return nil }
	t.Cleanup(func() {
		hostingerWaitForSSHReady = oldWaitForSSHReady
		hostingerRunSSHQuiet = oldRunSSHQuiet
	})

	statusLease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "vm-stopped", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if statusLease.Server.Status != "stopped" || statusLease.Server.Labels["state"] != "stopped" || api.startCalls != 0 {
		t.Fatalf("status lease=%#v starts=%v", statusLease, api.started)
	}
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "vm-stopped", StatusOnly: true, ReadyProbe: true}); err != nil {
		t.Fatal(err)
	}
	if api.startCalls != 0 {
		t.Fatalf("status readiness probe started stopped VPS: %v", api.started)
	}

	readOnly, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "vm-stopped"})
	if err != nil {
		t.Fatal(err)
	}
	if readOnly.Server.Status != "stopped" || api.startCalls != 0 {
		t.Fatalf("read-only resolve=%#v starts=%v", readOnly, api.started)
	}
	if err := core.ClaimLeaseTargetForConfig(readOnly.LeaseID, "blue", cfg, readOnly.Server, core.SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}

	prepared, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "vm-stopped", Prepare: true})
	if err != nil {
		t.Fatal(err)
	}
	if api.startCalls != 1 || api.started[0] != "vm-stopped" || prepared.SSH.Host == "" || waitCalls != 1 {
		t.Fatalf("prepared=%#v starts=%v", prepared, api.started)
	}

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID:   "vm-stopped",
		Repo: core.Repo{Root: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if api.startCalls != 1 || lease.SSH.Host == "" || waitCalls != 1 {
		t.Fatalf("lease=%#v starts=%v", lease, api.started)
	}
}

func TestResolveRequiresStoredSSHKeyOrExplicitAlternate(t *testing.T) {
	isolateHostingerTestState(t)
	leaseID := "cbx_missingkey123"
	vm := hostingerVM{
		ID:       "vm-missing-key",
		Hostname: "crabbox-missingkey-missingkey123",
		State:    "running",
		IPv4:     hostingerIPAddresses{"203.0.113.91"},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Hostinger.APIToken = "token"
	applyDefaults(&cfg)
	server := hostingerServer(vm, leaseID, "missingkey", cfg, true)
	if err := core.ClaimLeaseTargetForConfig(leaseID, "missingkey", cfg, server, core.SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}

	api := &fakeAPI{vms: []hostingerVM{vm}}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api
	backend.skipSSHWait = true
	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID}); err == nil || !strings.Contains(err.Error(), "stored SSH key is missing") {
		t.Fatalf("Resolve err=%v", err)
	}

	cfg.SSHKey = "/tmp/hostinger-explicit-key"
	alternateBackend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	alternateBackend.client = api
	alternateBackend.skipSSHWait = true
	lease, err := alternateBackend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Key != cfg.SSHKey {
		t.Fatalf("SSH key=%q want %q", lease.SSH.Key, cfg.SSHKey)
	}
}

func TestResolveRestoresOwnedClaimWhenRefreshFails(t *testing.T) {
	isolateHostingerTestState(t)
	leaseID := "cbx_restore123456"
	repoA := filepath.Join(t.TempDir(), "repo-a")
	repoB := filepath.Join(t.TempDir(), "repo-b")
	vm := hostingerVM{
		ID:       "vm-restore",
		Hostname: "crabbox-restore-restore123456",
		State:    "running",
		IPv4:     hostingerIPAddresses{"203.0.113.92"},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.SSHKey = "/tmp/hostinger-explicit-key"
	cfg.Hostinger.APIToken = "token"
	applyDefaults(&cfg)
	cfg.Pond = "alpha"
	server := hostingerServer(vm, leaseID, "restore", cfg, true)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "restore", cfg, server, core.SSHTarget{}, repoA, time.Hour, true); err != nil {
		t.Fatal(err)
	}
	previous, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("previous=%#v ok=%v err=%v", previous, ok, err)
	}

	api := &fakeAPI{
		vms:          []hostingerVM{vm},
		getErrAtCall: 2,
		getErr:       errors.New("refresh unavailable"),
	}
	cfg.Pond = "beta"
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api
	backend.skipSSHWait = true
	_, err = backend.Resolve(context.Background(), core.ResolveRequest{
		ID:      leaseID,
		Repo:    core.Repo{Root: repoB},
		Reclaim: true,
	})
	if err == nil || !strings.Contains(err.Error(), "refresh claimed vps") {
		t.Fatalf("Resolve err=%v", err)
	}
	restored, ok, claimErr := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if claimErr != nil || !ok || !reflect.DeepEqual(restored, previous) {
		t.Fatalf("restored=%#v previous=%#v ok=%v err=%v", restored, previous, ok, claimErr)
	}
}

func TestResolveRestoresStoredSSHConfiguration(t *testing.T) {
	isolateHostingerTestState(t)
	vm := hostingerVM{
		ID:       "vm-custom-ssh",
		Hostname: "crabbox-custom-abcdef123456",
		State:    "running",
		IPv4:     hostingerIPAddresses{"203.0.113.90"},
	}
	storedCfg := core.BaseConfig()
	storedCfg.Provider = providerName
	storedCfg.Hostinger.User = "ubuntu"
	storedCfg.Hostinger.WorkRoot = "/opt/crabbox/project"
	applyDefaults(&storedCfg)
	server := hostingerServer(vm, "cbx_abcdef123456", "custom", storedCfg, true)
	if server.Labels["ssh_user"] != "ubuntu" || server.Labels["work_root"] != "/opt/crabbox/project" {
		t.Fatalf("stored labels=%#v", server.Labels)
	}
	if err := core.ClaimLeaseTargetForConfig(
		"cbx_abcdef123456",
		"custom",
		storedCfg,
		server,
		core.SSHTarget{},
		time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKey("cbx_abcdef123456"); err != nil {
		t.Fatal(err)
	}

	api := &fakeAPI{vms: []hostingerVM{vm}}
	backend := NewLeaseBackend(Provider{}.Spec(), core.Config{
		Provider: providerName,
		Hostinger: core.HostingerConfig{
			APIToken: "token",
		},
	}, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	oldRunSSHQuiet := hostingerRunSSHQuiet
	var bootstrapTarget SSHTarget
	var bootstrapCommand string
	hostingerRunSSHQuiet = func(_ context.Context, target SSHTarget, command string) error {
		bootstrapTarget = target
		bootstrapCommand = command
		return nil
	}
	t.Cleanup(func() { hostingerRunSSHQuiet = oldRunSSHQuiet })

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID:      "cbx_abcdef123456",
		Prepare: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.User != "ubuntu" ||
		bootstrapTarget.User != "ubuntu" ||
		!strings.Contains(lease.SSH.ReadyCheck, "test -w '/opt/crabbox/project'") ||
		!strings.Contains(bootstrapCommand, "/opt/crabbox/project") {
		t.Fatalf("lease=%#v bootstrapTarget=%#v command=%q", lease, bootstrapTarget, bootstrapCommand)
	}

	explicitCfg := core.BaseConfig()
	explicitCfg.Provider = providerName
	explicitCfg.Hostinger.APIToken = "token"
	explicitCfg.Hostinger.User = "alice"
	explicitCfg.Hostinger.WorkRoot = "/home/alice/crabbox"
	applyDefaults(&explicitCfg)
	core.MarkHostingerUserExplicit(&explicitCfg)
	core.MarkHostingerWorkRootExplicit(&explicitCfg)
	explicitBackend := NewLeaseBackend(Provider{}.Spec(), explicitCfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	explicitBackend.client = api
	explicitBackend.skipSSHWait = true
	explicitLease, err := explicitBackend.Resolve(context.Background(), core.ResolveRequest{ID: "cbx_abcdef123456"})
	if err != nil {
		t.Fatal(err)
	}
	if explicitLease.SSH.User != "alice" || !strings.Contains(explicitLease.SSH.ReadyCheck, "test -w '/home/alice/crabbox'") {
		t.Fatalf("explicit lease=%#v", explicitLease)
	}
	if explicitLease.Server.Labels["ssh_user"] != "alice" || explicitLease.Server.Labels["work_root"] != "/home/alice/crabbox" {
		t.Fatalf("explicit labels=%#v", explicitLease.Server.Labels)
	}

	userOnlyCfg := core.BaseConfig()
	userOnlyCfg.Provider = providerName
	userOnlyCfg.Hostinger.APIToken = "token"
	userOnlyCfg.Hostinger.User = "alice"
	applyDefaults(&userOnlyCfg)
	core.MarkHostingerUserExplicit(&userOnlyCfg)
	userOnlyBackend := NewLeaseBackend(Provider{}.Spec(), userOnlyCfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	userOnlyBackend.client = api
	userOnlyBackend.skipSSHWait = true
	userOnlyLease, err := userOnlyBackend.Resolve(context.Background(), core.ResolveRequest{ID: "cbx_abcdef123456"})
	if err != nil {
		t.Fatal(err)
	}
	if userOnlyLease.SSH.User != "alice" || userOnlyLease.Server.Labels["work_root"] != "/home/alice/crabbox" {
		t.Fatalf("user-only lease=%#v", userOnlyLease)
	}

	sameUserCfg := core.BaseConfig()
	sameUserCfg.Provider = providerName
	sameUserCfg.Hostinger.APIToken = "token"
	sameUserCfg.Hostinger.User = "ubuntu"
	applyDefaults(&sameUserCfg)
	core.MarkHostingerUserExplicit(&sameUserCfg)
	sameUserBackend := NewLeaseBackend(Provider{}.Spec(), sameUserCfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	sameUserBackend.client = api
	sameUserBackend.skipSSHWait = true
	sameUserLease, err := sameUserBackend.Resolve(context.Background(), core.ResolveRequest{ID: "cbx_abcdef123456"})
	if err != nil {
		t.Fatal(err)
	}
	if sameUserLease.Server.Labels["work_root"] != "/opt/crabbox/project" {
		t.Fatalf("same-user lease=%#v", sameUserLease)
	}
}

func TestResolveRollsBackRestartWhenPreparationFails(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-stopped", Hostname: "crabbox-blue-abcdef123456", State: "stopped"}},
	}
	cfg := core.Config{
		Provider: providerName,
		SSHKey:   "/tmp/test-key",
		Hostinger: core.HostingerConfig{
			APIToken: "token",
		},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	ctx, cancel := context.WithCancel(context.Background())
	oldRunSSHQuiet := hostingerRunSSHQuiet
	oldWaitForSSHReady := hostingerWaitForSSHReady
	oldSleep := hostingerSleep
	hostingerWaitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		return nil
	}
	hostingerRunSSHQuiet = func(context.Context, SSHTarget, string) error {
		cancel()
		return errors.New("bootstrap failed")
	}
	hostingerSleep = func(time.Duration) {}
	t.Cleanup(func() {
		hostingerRunSSHQuiet = oldRunSSHQuiet
		hostingerWaitForSSHReady = oldWaitForSSHReady
		hostingerSleep = oldSleep
	})

	_, err := backend.Resolve(ctx, core.ResolveRequest{
		ID:      "vm-stopped",
		Repo:    core.Repo{Root: t.TempDir()},
		Prepare: true,
	})
	if err == nil || !strings.Contains(err.Error(), "restart rollback=stopped") {
		t.Fatalf("Resolve err=%v", err)
	}
	if api.startCalls != 1 || api.stopCalls != 1 || len(api.vms) != 1 || !api.vms[0].Stopped() {
		t.Fatalf("starts=%v stops=%v vms=%#v", api.started, api.stopped, api.vms)
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil || len(claims) != 1 || claims[0].Labels["state"] != "stopped" {
		t.Fatalf("rollback claims=%#v err=%v", claims, claimErr)
	}
}

func TestResolveDoesNotRollbackRestartAfterClaimChanges(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-stopped", Hostname: "crabbox-blue-abcdef123456", State: "stopped"}},
	}
	cfg := core.Config{
		Provider: providerName,
		SSHKey:   "/tmp/test-key",
		Hostinger: core.HostingerConfig{
			APIToken: "token",
		},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	ctx, cancel := context.WithCancel(context.Background())
	oldRunSSHQuiet := hostingerRunSSHQuiet
	oldWaitForSSHReady := hostingerWaitForSSHReady
	oldSleep := hostingerSleep
	hostingerWaitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		return nil
	}
	hostingerRunSSHQuiet = func(context.Context, SSHTarget, string) error {
		claims, err := core.ListLeaseClaims()
		if err != nil || len(claims) != 1 {
			return fmt.Errorf("claims=%#v err=%v", claims, err)
		}
		labels := make(map[string]string, len(claims[0].Labels))
		for key, value := range claims[0].Labels {
			labels[key] = value
		}
		labels["state"] = "running"
		labels["expires_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(claims[0].LeaseID, claims[0], labels); err != nil {
			return err
		}
		cancel()
		return errors.New("bootstrap failed")
	}
	hostingerSleep = func(time.Duration) {}
	t.Cleanup(func() {
		hostingerRunSSHQuiet = oldRunSSHQuiet
		hostingerWaitForSSHReady = oldWaitForSSHReady
		hostingerSleep = oldSleep
	})

	_, err := backend.Resolve(ctx, core.ResolveRequest{
		ID:      "vm-stopped",
		Repo:    core.Repo{Root: t.TempDir()},
		Prepare: true,
	})
	if err == nil || !strings.Contains(err.Error(), "restart rollback skipped") || !strings.Contains(err.Error(), "claim changed; retry") {
		t.Fatalf("Resolve err=%v", err)
	}
	if api.startCalls != 1 || api.stopCalls != 0 || len(api.vms) != 1 || !api.vms[0].Ready() {
		t.Fatalf("starts=%v stops=%v vms=%#v", api.started, api.stopped, api.vms)
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil || len(claims) != 1 || claims[0].Labels["state"] != "running" {
		t.Fatalf("claims=%#v err=%v", claims, claimErr)
	}
}

func TestResolvePreservesProvisioningClaimWhenRestartStopFails(t *testing.T) {
	isolateHostingerTestState(t)
	leaseID := "cbx_stopfail123456"
	repoA := filepath.Join(t.TempDir(), "repo-a")
	repoB := filepath.Join(t.TempDir(), "repo-b")
	cfg := core.Config{
		Provider:    providerName,
		SSHKey:      "/tmp/test-key",
		IdleTimeout: time.Hour,
		Hostinger: core.HostingerConfig{
			APIToken: "token",
		},
	}
	vm := hostingerVM{ID: "vm-stop-fail", Hostname: "crabbox-stopfail-stopfail123456", State: "stopped"}
	server := hostingerServer(vm, leaseID, "stopfail", cfg, true)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "stopfail", cfg, server, core.SSHTarget{}, repoA, time.Hour, true); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{
		vms:     []hostingerVM{vm},
		stopErr: errors.New("stop unavailable"),
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	ctx, cancel := context.WithCancel(context.Background())
	oldRunSSHQuiet := hostingerRunSSHQuiet
	oldWaitForSSHReady := hostingerWaitForSSHReady
	oldSleep := hostingerSleep
	hostingerWaitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		return nil
	}
	hostingerRunSSHQuiet = func(context.Context, SSHTarget, string) error {
		cancel()
		return errors.New("bootstrap failed")
	}
	hostingerSleep = func(time.Duration) {}
	t.Cleanup(func() {
		hostingerRunSSHQuiet = oldRunSSHQuiet
		hostingerWaitForSSHReady = oldWaitForSSHReady
		hostingerSleep = oldSleep
	})

	_, err := backend.Resolve(ctx, core.ResolveRequest{
		ID:      leaseID,
		Repo:    core.Repo{Root: repoB},
		Reclaim: true,
		Prepare: true,
	})
	if err == nil || !strings.Contains(err.Error(), "restart rollback skipped") || !strings.Contains(err.Error(), "stop unavailable") {
		t.Fatalf("Resolve err=%v", err)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if claimErr != nil || !ok {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	if claim.RepoRoot != repoB || claim.Labels["state"] != "provisioning" {
		t.Fatalf("failed stop restored stale claim: %#v", claim)
	}
	if len(api.vms) != 1 || !api.vms[0].Ready() {
		t.Fatalf("VM state=%#v", api.vms)
	}
}

func TestResolveRestoresOwnershipWithoutStaleRunningState(t *testing.T) {
	isolateHostingerTestState(t)
	leaseID := "cbx_stalerun123456"
	repoA := filepath.Join(t.TempDir(), "repo-a")
	repoB := filepath.Join(t.TempDir(), "repo-b")
	cfg := core.Config{
		Provider:    providerName,
		SSHKey:      "/tmp/test-key",
		IdleTimeout: time.Hour,
		Hostinger: core.HostingerConfig{
			APIToken: "token",
		},
	}
	vm := hostingerVM{ID: "vm-stale-running", Hostname: "crabbox-stalerun-stalerun123456", State: "stopped"}
	server := hostingerServer(vm, leaseID, "stalerun", cfg, true)
	server.Labels["state"] = "running"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "stalerun", cfg, server, core.SSHTarget{Host: "203.0.113.99", Port: "22"}, repoA, time.Hour, true); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{vms: []hostingerVM{vm}}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	ctx, cancel := context.WithCancel(context.Background())
	oldRunSSHQuiet := hostingerRunSSHQuiet
	oldWaitForSSHReady := hostingerWaitForSSHReady
	oldSleep := hostingerSleep
	hostingerWaitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		return nil
	}
	hostingerRunSSHQuiet = func(context.Context, SSHTarget, string) error {
		cancel()
		return errors.New("bootstrap failed")
	}
	hostingerSleep = func(time.Duration) {}
	t.Cleanup(func() {
		hostingerRunSSHQuiet = oldRunSSHQuiet
		hostingerWaitForSSHReady = oldWaitForSSHReady
		hostingerSleep = oldSleep
	})

	_, err := backend.Resolve(ctx, core.ResolveRequest{
		ID:      leaseID,
		Repo:    core.Repo{Root: repoB},
		Reclaim: true,
		Prepare: true,
	})
	if err == nil || !strings.Contains(err.Error(), "restart rollback=stopped") {
		t.Fatalf("Resolve err=%v", err)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if claimErr != nil || !ok {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	if claim.RepoRoot != repoA || claim.Labels["state"] != "stopped" || claim.SSHHost != "" {
		t.Fatalf("restored claim=%#v", claim)
	}
	if len(api.vms) != 1 || !api.vms[0].Stopped() {
		t.Fatalf("VM state=%#v", api.vms)
	}
}

func TestResolveRefreshesExpiredClaimBeforeRestart(t *testing.T) {
	isolateHostingerTestState(t)
	repoRoot := t.TempDir()
	leaseID := "cbx_expired123456"
	cfg := core.Config{
		Provider:    providerName,
		SSHKey:      "/tmp/test-key",
		IdleTimeout: time.Hour,
		Hostinger: core.HostingerConfig{
			APIToken: "token",
		},
	}
	labels := core.DirectLeaseLabels(cfg, leaseID, "expired", providerName, "", false, time.Now().Add(-48*time.Hour))
	labels["state"] = "stopped"
	server := core.Server{CloudID: "vm-stopped", Provider: providerName, Name: "crabbox-expired-expired123456", Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "expired", cfg, server, core.SSHTarget{}, repoRoot, time.Hour, true); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-stopped", Hostname: server.Name, State: "stopped"}},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api
	backend.skipSSHWait = true

	if _, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, Repo: core.Repo{Root: repoRoot}}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok || claim.Labels["state"] != "running" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
	expiresUnix, err := strconv.ParseInt(claim.Labels["expires_at"], 10, 64)
	if err != nil || !time.Unix(expiresUnix, 0).After(time.Now()) {
		t.Fatalf("expires_at=%q err=%v", claim.Labels["expires_at"], err)
	}
}

func TestResolveStatusReadyProbeAllowsRunningVMWithoutPublicIP(t *testing.T) {
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-pending-ip", Hostname: "crabbox-blue-abcdef123456", State: "running"}},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), core.Config{
		Hostinger: core.HostingerConfig{APIToken: "token"},
	}, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID:         "vm-pending-ip",
		StatusOnly: true,
		ReadyProbe: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Status != "running" || lease.Server.PublicNet.IPv4.IP != "" || lease.SSH.Host != "" {
		t.Fatalf("lease=%#v", lease)
	}
	if api.startCalls != 0 {
		t.Fatalf("status probe started VPS: %v", api.started)
	}
}

func TestResolveRejectsInvalidReleaseActionBeforeStartingVM(t *testing.T) {
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-stopped", Hostname: "crabbox-blue-abcdef123456", State: "stopped"}},
	}
	cfg := core.Config{
		Hostinger: core.HostingerConfig{
			APIToken:      "token",
			ReleaseAction: "delete",
		},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "vm-stopped"})
	if err == nil || !strings.Contains(err.Error(), "release action must be stop") {
		t.Fatalf("Resolve err=%v", err)
	}
	if api.startCalls != 0 {
		t.Fatalf("Resolve started VPS with invalid release action: %v", api.started)
	}
}

func TestResolveChecksRepoClaimBeforeStartingVM(t *testing.T) {
	isolateHostingerTestState(t)
	leaseID := "cbx_abcdef123456"
	cfg := core.Config{
		Provider: providerName,
		SSHKey:   "/tmp/test-key",
		Hostinger: core.HostingerConfig{
			APIToken: "token",
		},
	}
	server := core.Server{
		CloudID:  "vm-stopped",
		Provider: providerName,
		Name:     "crabbox-blue-abcdef123456",
		Labels:   core.DirectLeaseLabels(cfg, leaseID, "blue", providerName, "", true, time.Now()),
	}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "blue", cfg, server, core.SSHTarget{}, filepath.Join(t.TempDir(), "first"), time.Hour, true); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-stopped", Hostname: "crabbox-blue-abcdef123456", State: "stopped"}},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID:   leaseID,
		Repo: core.Repo{Root: filepath.Join(t.TempDir(), "second")},
	})
	if err == nil || !strings.Contains(err.Error(), "is claimed by repo") {
		t.Fatalf("Resolve err=%v", err)
	}
	if api.startCalls != 0 {
		t.Fatalf("Resolve started VPS before claim validation: %v", api.started)
	}
}

func TestResolveRefreshesVMStateAfterClaim(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-refresh", Hostname: "crabbox-blue-abcdef123456", State: "stopped"}},
		getSequence: []hostingerVM{
			{ID: "vm-refresh", Hostname: "crabbox-blue-abcdef123456", State: "running", IPv4: hostingerIPAddresses{"203.0.113.61"}},
			{ID: "vm-refresh", Hostname: "crabbox-blue-abcdef123456", State: "stopped"},
		},
	}
	cfg := core.Config{
		Provider: providerName,
		SSHKey:   "/tmp/test-key",
		Hostinger: core.HostingerConfig{
			APIToken: "token",
		},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api
	backend.skipSSHWait = true

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID:      "vm-refresh",
		Repo:    core.Repo{Root: t.TempDir()},
		Reclaim: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if api.startCalls != 1 || lease.Server.CloudID != "vm-refresh" || lease.SSH.Host == "" {
		t.Fatalf("lease=%#v starts=%v", lease, api.started)
	}
}

func TestResolveRejectsAmbiguousHostingerSlug(t *testing.T) {
	api := &fakeAPI{
		vms: []hostingerVM{
			{ID: "vm-one", Hostname: "crabbox-shared-111111111111", State: "running", IPv4: hostingerIPAddresses{"203.0.113.41"}},
			{ID: "vm-two", Hostname: "crabbox-shared-222222222222", State: "running", IPv4: hostingerIPAddresses{"203.0.113.42"}},
		},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), core.Config{
		Hostinger: core.HostingerConfig{APIToken: "token"},
	}, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "shared"})
	if err == nil || !strings.Contains(err.Error(), `slug "shared" matches multiple active leases`) {
		t.Fatalf("Resolve err=%v", err)
	}
	if api.startCalls != 0 {
		t.Fatalf("ambiguous resolve started VPS: %v", api.started)
	}
}

func TestReleaseRetainsClaimUntilStopConfirmed(t *testing.T) {
	isolateHostingerTestState(t)
	leaseID := "cbx_abcdef123456"
	cfg := core.Config{
		Provider: providerName,
		Hostinger: core.HostingerConfig{
			APIToken: "token",
		},
	}
	server := core.Server{
		CloudID:  "vm-running",
		Provider: providerName,
		Name:     "crabbox-blue-abcdef123456",
		Labels:   core.DirectLeaseLabels(cfg, leaseID, "blue", providerName, "", true, time.Now()),
	}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "blue", cfg, server, core.SSHTarget{}, t.TempDir(), time.Hour, true); err != nil {
		t.Fatal(err)
	}
	keyPath, _, keyErr := core.EnsureTestboxKey(leaseID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	api := &fakeAPI{
		vms:                     []hostingerVM{{ID: "vm-running", Hostname: "crabbox-blue-abcdef123456", State: "running", IPv4: hostingerIPAddresses{"203.0.113.60"}}},
		stopLeavesRunning:       true,
		getWaitForContextAtCall: 2,
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api
	oldTimeout := hostingerStopWaitTimeout
	hostingerStopWaitTimeout = time.Nanosecond
	t.Cleanup(func() { hostingerStopWaitTimeout = oldTimeout })

	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{LeaseID: leaseID, Server: server},
	})
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for hostinger vps vm-running to stop") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if api.stopCalls != 1 {
		t.Fatalf("stopCalls=%d", api.stopCalls)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if claimErr != nil || !ok || claim.CloudID != "vm-running" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("failed release removed recovery key: %v", statErr)
	}
}

func TestReleaseRejectsChangedClaimBeforeStop(t *testing.T) {
	isolateHostingerTestState(t)
	leaseID := "cbx_abcdef123456"
	cfg := core.Config{
		Provider: providerName,
		Hostinger: core.HostingerConfig{
			APIToken: "token",
		},
	}
	server := core.Server{
		CloudID:  "vm-running",
		Provider: providerName,
		Name:     "crabbox-blue-abcdef123456",
		Labels:   core.DirectLeaseLabels(cfg, leaseID, "blue", providerName, "", true, time.Now()),
	}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "blue", cfg, server, core.SSHTarget{}, t.TempDir(), time.Hour, true); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-running", Hostname: "crabbox-blue-abcdef123456", State: "running", IPv4: hostingerIPAddresses{"203.0.113.60"}}},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	oldUpdate := updateLeaseClaimEndpointIfUnchangedAfter
	updateLeaseClaimEndpointIfUnchangedAfter = func(id string, expected core.LeaseClaim, server core.Server, target core.SSHTarget, action func() error) (core.LeaseClaim, error) {
		refreshed := make(map[string]string, len(expected.Labels))
		for key, value := range expected.Labels {
			refreshed[key] = value
		}
		refreshed["state"] = "running"
		refreshed["expires_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(id, expected, refreshed); err != nil {
			return core.LeaseClaim{}, err
		}
		return oldUpdate(id, expected, server, target, action)
	}
	t.Cleanup(func() { updateLeaseClaimEndpointIfUnchangedAfter = oldUpdate })

	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{LeaseID: leaseID, Server: server},
	})
	if err == nil || !strings.Contains(err.Error(), "claim changed; retry") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if api.stopCalls != 0 {
		t.Fatalf("release stopped renewed lease: %v", api.stopped)
	}
}

func TestResolveReleaseRejectsUnownedHostingerVM(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-production", Hostname: "crabbox-blue-abcdef123456", State: "running", IPv4: hostingerIPAddresses{"203.0.113.50"}}},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), core.Config{
		Hostinger: core.HostingerConfig{APIToken: "token"},
	}, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	list, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("default list included unclaimed hostname-pattern VPS: %#v", list)
	}
	all, err := backend.List(context.Background(), core.ListRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("all list len=%d, want 1", len(all))
	}

	_, err = backend.Resolve(context.Background(), core.ResolveRequest{ID: "vm-production", ReleaseOnly: true})
	if err == nil || !strings.Contains(err.Error(), "refusing to stop unowned hostinger vps") {
		t.Fatalf("Resolve err=%v", err)
	}
	if api.stopCalls != 0 {
		t.Fatalf("unowned resolve stopped VPS: %v", api.stopped)
	}

	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{
			LeaseID: "cbx_hostinger_vm-production",
			Server:  core.Server{CloudID: "vm-production", Name: "crabbox-blue-abcdef123456", Provider: providerName},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to stop unowned hostinger vps") {
		t.Fatalf("ReleaseLease err=%v", err)
	}
	if api.stopCalls != 0 {
		t.Fatalf("unowned release stopped VPS: %v", api.stopped)
	}
}

func TestFailedHostingerAdoptionRemainsNonDestructiveUntilSSHValidation(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-production", Hostname: "crabbox-blue-abcdef123456", State: "running", IPv4: hostingerIPAddresses{"203.0.113.50"}}},
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Hostinger.APIToken = "token"
	cfg.SSHKey = filepath.Join(t.TempDir(), "adoption-key")
	core.MarkSSHKeyExplicit(&cfg)
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api
	repoRoot := t.TempDir()

	oldRunSSHQuiet := hostingerRunSSHQuiet
	oldSleep := hostingerSleep
	ctx, cancel := context.WithCancel(context.Background())
	hostingerRunSSHQuiet = func(context.Context, SSHTarget, string) error {
		cancel()
		return errors.New("ssh validation failed")
	}
	hostingerSleep = func(time.Duration) {}
	t.Cleanup(func() {
		hostingerRunSSHQuiet = oldRunSSHQuiet
		hostingerSleep = oldSleep
	})

	_, err := backend.Resolve(ctx, core.ResolveRequest{
		ID:      "crabbox-blue-abcdef123456",
		Repo:    core.Repo{Root: repoRoot},
		Prepare: true,
	})
	if err == nil {
		t.Fatalf("Resolve error=%v", err)
	}
	claims, claimErr := core.ListLeaseClaims()
	if claimErr != nil || len(claims) != 1 || !hostingerAdoptionPending(claims[0]) {
		t.Fatalf("pending claims=%#v err=%v", claims, claimErr)
	}
	leaseID := claims[0].LeaseID
	owned, listErr := backend.List(context.Background(), core.ListRequest{})
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(owned) != 0 {
		t.Fatalf("pending adoption appeared owned: %#v", owned)
	}
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{LeaseID: leaseID, Server: core.Server{CloudID: "vm-production", Provider: providerName}},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to stop unowned hostinger vps") {
		t.Fatalf("ReleaseLease error=%v", err)
	}
	if api.stopCalls != 0 {
		t.Fatalf("failed adoption stopped VPS: %v", api.stopped)
	}

	hostingerRunSSHQuiet = func(context.Context, SSHTarget, string) error { return nil }
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID:      "crabbox-blue-abcdef123456",
		Repo:    core.Repo{Root: repoRoot},
		Prepare: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if claimErr != nil || !ok || hostingerAdoptionPending(claim) {
		t.Fatalf("adopted claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if api.stopCalls != 1 || api.stopped[0] != "vm-production" {
		t.Fatalf("stops=%v", api.stopped)
	}
}

func TestUnclaimedHostnameCollisionDoesNotRebindClaim(t *testing.T) {
	isolateHostingerTestState(t)
	leaseID := "cbx_abcdef123456"
	cfg := core.Config{
		Provider: providerName,
		SSHKey:   "/tmp/test-key",
		Hostinger: core.HostingerConfig{
			APIToken:       "token",
			HostnamePrefix: "crabbox",
		},
	}
	original := core.Server{CloudID: "vm-original", Provider: providerName, Name: "crabbox-blue-abcdef123456"}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "blue", cfg, original, core.SSHTarget{}, t.TempDir(), time.Hour, true); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-collision", Hostname: "crabbox-blue-abcdef123456", State: "running", IPv4: hostingerIPAddresses{"203.0.113.50"}}},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api
	backend.skipSSHWait = true

	resolved, err := backend.Resolve(context.Background(), core.ResolveRequest{
		ID:   "vm-collision",
		Repo: core.Repo{Root: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.LeaseID != "cbx_hostinger_vm-collision" {
		t.Fatalf("collision lease id=%q", resolved.LeaseID)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok || claim.CloudID != "vm-original" {
		t.Fatalf("original claim rebound: claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestResolveRejectsDuplicatePendingRecoveryHostname(t *testing.T) {
	isolateHostingerTestState(t)
	leaseID := "cbx_abcdef123456"
	hostname := "crabbox-pending-abcdef123456"
	cfg := core.Config{
		Provider: providerName,
		Hostinger: core.HostingerConfig{
			APIToken:       "token",
			HostnamePrefix: "crabbox",
		},
	}
	labels := core.DirectLeaseLabels(cfg, leaseID, "pending", providerName, "", true, time.Now())
	labels[hostingerRecoveryLabel] = hostingerRecoveryAmbiguous
	labels[hostingerRecoveryHostnameLabel] = hostname
	if err := core.ClaimLeaseTargetForRepoConfig(
		leaseID,
		"pending",
		cfg,
		core.Server{Provider: providerName, Name: hostname, Labels: labels},
		core.SSHTarget{},
		t.TempDir(),
		time.Hour,
		true,
	); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{
		vms: []hostingerVM{
			{ID: "vm-one", Hostname: hostname, State: "running", IPv4: hostingerIPAddresses{"203.0.113.51"}},
			{ID: "vm-two", Hostname: hostname, State: "running", IPv4: hostingerIPAddresses{"203.0.113.52"}},
		},
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "multiple Hostinger VPSs match pending recovery hostname") {
		t.Fatalf("Resolve err=%v", err)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels[hostingerRecoveryLabel] != hostingerRecoveryAmbiguous {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
}

func TestResolveUsesStableCloudIDFromClaim(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "1750635", Hostname: "renamed-manually", State: "running", IPv4: hostingerIPAddresses{"203.0.113.44"}}},
	}
	cfg := core.Config{
		Provider: "hostinger",
		SSHKey:   "/tmp/test-key",
		Hostinger: core.HostingerConfig{
			APIToken:       "token",
			HostnamePrefix: "different-prefix",
		},
	}
	leaseID := "cbx_abcdef123456"
	server := core.Server{CloudID: "1750635", Provider: providerName, Labels: core.DirectLeaseLabels(cfg, leaseID, "stable", providerName, "", true, time.Now())}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "stable", cfg, server, core.SSHTarget{}, t.TempDir(), time.Hour, true); err != nil {
		t.Fatal(err)
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "1750635" || lease.LeaseID != leaseID || lease.Server.Labels["slug"] != "stable" {
		t.Fatalf("lease=%#v", lease)
	}
	if api.getCalls != 1 || api.listCalls != 0 {
		t.Fatalf("getCalls=%d listCalls=%d, want stable-id direct get", api.getCalls, api.listCalls)
	}

	direct, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "1750635"})
	if err != nil {
		t.Fatal(err)
	}
	if direct.LeaseID != leaseID || direct.Server.Labels["slug"] != "stable" {
		t.Fatalf("direct lease=%#v", direct)
	}

	servers, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].CloudID != "1750635" ||
		servers[0].Labels["lease"] != leaseID || servers[0].Labels["slug"] != "stable" {
		t.Fatalf("servers=%#v", servers)
	}
}

func TestTouchPersistsRunningStateForCleanup(t *testing.T) {
	isolateHostingerTestState(t)
	leaseID := "cbx_abcdef123456"
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-crabbox", Hostname: "crabbox-green-abcdef123456", State: "running", IPv4: hostingerIPAddresses{"203.0.113.32"}}},
	}
	cfg := core.Config{
		Provider: providerName,
		Hostinger: core.HostingerConfig{
			APIToken:       "token",
			HostnamePrefix: "crabbox",
		},
	}
	labels := core.DirectLeaseLabels(cfg, leaseID, "green", providerName, "", false, time.Now().Add(-2*time.Hour))
	labels["state"] = "expired"
	server := core.Server{CloudID: "vm-crabbox", Provider: providerName, Name: "crabbox-green-abcdef123456", Labels: labels}
	target := core.SSHTarget{Host: "203.0.113.32", Port: "22"}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "green", cfg, server, target, t.TempDir(), time.Hour, true); err != nil {
		t.Fatal(err)
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	touched, err := backend.Touch(context.Background(), core.TouchRequest{
		Lease: core.LeaseTarget{LeaseID: leaseID, Server: server, SSH: target},
		State: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	if touched.Labels["state"] != "running" {
		t.Fatalf("touched=%#v", touched.Labels)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok || claim.Labels["state"] != "running" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if api.stopCalls != 0 {
		t.Fatalf("cleanup stopped running lease: %v", api.stopped)
	}
}

func TestCleanupRejectsChangedClaimBeforeStop(t *testing.T) {
	isolateHostingerTestState(t)
	leaseID := "cbx_abcdef123456"
	api := &fakeAPI{
		vms: []hostingerVM{{ID: "vm-crabbox", Hostname: "crabbox-green-abcdef123456", State: "running", IPv4: hostingerIPAddresses{"203.0.113.32"}}},
	}
	cfg := core.Config{
		Provider: providerName,
		Hostinger: core.HostingerConfig{
			APIToken:       "token",
			HostnamePrefix: "crabbox",
		},
	}
	labels := core.DirectLeaseLabels(cfg, leaseID, "green", providerName, "", false, time.Now().Add(-48*time.Hour))
	labels["state"] = "expired"
	server := core.Server{CloudID: "vm-crabbox", Provider: providerName, Name: "crabbox-green-abcdef123456", Labels: labels}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "green", cfg, server, core.SSHTarget{}, t.TempDir(), time.Hour, true); err != nil {
		t.Fatal(err)
	}
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*leaseBackend)
	backend.client = api

	oldUpdate := updateLeaseClaimEndpointIfUnchangedAfter
	updateLeaseClaimEndpointIfUnchangedAfter = func(id string, expected core.LeaseClaim, server core.Server, target core.SSHTarget, action func() error) (core.LeaseClaim, error) {
		refreshed := make(map[string]string, len(expected.Labels))
		for key, value := range expected.Labels {
			refreshed[key] = value
		}
		refreshed["state"] = "running"
		refreshed["expires_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(id, expected, refreshed); err != nil {
			return core.LeaseClaim{}, err
		}
		return oldUpdate(id, expected, server, target, action)
	}
	t.Cleanup(func() { updateLeaseClaimEndpointIfUnchangedAfter = oldUpdate })

	err := backend.Cleanup(context.Background(), core.CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "claim changed; retry") {
		t.Fatalf("Cleanup err=%v", err)
	}
	if api.stopCalls != 0 {
		t.Fatalf("cleanup stopped renewed lease: %v", api.stopped)
	}
	claim, ok, claimErr := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if claimErr != nil || !ok || claim.Labels["state"] != "running" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, claimErr)
	}
}

func TestCleanupSkipsManualHostingerVMs(t *testing.T) {
	isolateHostingerTestState(t)
	api := &fakeAPI{
		vms: []hostingerVM{
			{ID: "vm-manual", Hostname: "production-db", State: "running", IPv4: hostingerIPAddresses{"203.0.113.30"}},
			{ID: "vm-prefix", Hostname: "crabbox-production", State: "running", IPv4: hostingerIPAddresses{"203.0.113.31"}},
			{ID: "vm-collision", Hostname: "crabbox-red-abcdef123456", State: "running", IPv4: hostingerIPAddresses{"203.0.113.33"}},
			{ID: "vm-crabbox", Hostname: "crabbox-green-abcdef123456", State: "running", IPv4: hostingerIPAddresses{"203.0.113.32"}},
		},
	}
	cfg := core.Config{Hostinger: core.HostingerConfig{
		APIToken:       "token",
		HostnamePrefix: "crabbox",
	}}
	var stderr bytes.Buffer
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: &stderr}).(*leaseBackend)
	backend.client = api

	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if api.stopCalls != 0 {
		t.Fatalf("cleanup without local claim stopped VMs: %v", api.stopped)
	}
	stderrText := stderr.String()
	for _, want := range []string{"reason=not-crabbox-owned", "reason=no-local-cleanup-claim"} {
		if !strings.Contains(stderrText, want) {
			t.Fatalf("cleanup stderr missing %q:\n%s", want, stderrText)
		}
	}

	labels := core.DirectLeaseLabels(core.Config{Provider: providerName, TargetOS: core.TargetLinux}, "cbx_abcdef123456", "green", providerName, "", false, time.Now().Add(-48*time.Hour))
	labels["state"] = "expired"
	if err := core.ClaimLeaseForRepoProvider("cbx_abcdef123456", "green", providerName, t.TempDir(), time.Hour, true); err != nil {
		t.Fatal(err)
	}
	if err := core.UpdateLeaseClaimEndpoint("cbx_abcdef123456", core.Server{CloudID: "vm-crabbox", Provider: providerName, Labels: labels}, core.SSHTarget{Host: "203.0.113.32", Port: "22"}); err != nil {
		t.Fatal(err)
	}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if api.stopCalls != 1 || api.stopped[0] != "vm-crabbox" {
		t.Fatalf("stops=%v", api.stopped)
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider("cbx_abcdef123456", providerName)
	if err != nil || !ok || claim.Labels["state"] != "stopped" {
		t.Fatalf("cleanup claim=%#v ok=%v err=%v", claim, ok, err)
	}
	if claim.SSHHost != "" || claim.SSHPort != 0 {
		t.Fatalf("cleanup claim endpoint=%s:%d want cleared", claim.SSHHost, claim.SSHPort)
	}
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if api.stopCalls != 1 {
		t.Fatalf("cleanup stopped already stopped VPS again: %v", api.stopped)
	}
}

func TestHostingerDefaultsPreserveWorkRootPrecedence(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Hostinger.User = "ubuntu"
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{}).(*leaseBackend)
	if backend.cfg.WorkRoot != "/home/ubuntu/crabbox" || backend.cfg.Hostinger.WorkRoot != "/home/ubuntu/crabbox" {
		t.Fatalf("per-user work root default not applied: %#v", backend.cfg.Hostinger)
	}

	cfg = core.BaseConfig()
	cfg.WorkRoot = "/tmp/other-provider"
	backend = NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{}).(*leaseBackend)
	if backend.cfg.WorkRoot != "/home/root/crabbox" || backend.cfg.Hostinger.WorkRoot != "/home/root/crabbox" {
		t.Fatalf("inherited another provider's derived work root: %#v", backend.cfg.Hostinger)
	}

	cfg = core.BaseConfig()
	cfg.WorkRoot = "/srv/crabbox"
	core.MarkWorkRootExplicit(&cfg)
	cfg.WorkRoot = "/tmp/other-provider"
	backend = NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{}).(*leaseBackend)
	if backend.cfg.WorkRoot != "/srv/crabbox" || backend.cfg.Hostinger.WorkRoot != "/srv/crabbox" {
		t.Fatalf("generic work root not inherited: %#v", backend.cfg.Hostinger)
	}

	cfg.Hostinger.WorkRoot = "/opt/hostinger"
	core.MarkHostingerWorkRootExplicit(&cfg)
	backend = NewLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{}).(*leaseBackend)
	if backend.cfg.WorkRoot != "/opt/hostinger" || backend.cfg.Hostinger.WorkRoot != "/opt/hostinger" {
		t.Fatalf("provider work root did not win: %#v", backend.cfg.Hostinger)
	}
}

type fakeAPI struct {
	vms                     []hostingerVM
	catalog                 []hostingerCatalogItem
	paymentMethods          []hostingerPaymentMethod
	templates               []hostingerTemplate
	dataCenters             []hostingerDataCenter
	purchase                hostingerPurchaseInput
	setup                   hostingerSetupInput
	purchaseErrBeforeCreate error
	purchaseErrAfterCreate  error
	purchaseCalls           int
	setupCalls              int
	listCalls               int
	getCalls                int
	startCalls              int
	started                 []string
	stopCalls               int
	stopped                 []string
	stopLeavesRunning       bool
	stopErr                 error
	getSequence             []hostingerVM
	getErrAtCall            int
	getErr                  error
	getWaitForContextAtCall int
}

func (f *fakeAPI) ListCatalog(context.Context) ([]hostingerCatalogItem, error) {
	if f.catalog != nil {
		return append([]hostingerCatalogItem(nil), f.catalog...), nil
	}
	return []hostingerCatalogItem{{
		ID:       "hostingercom-vps-kvm2",
		Name:     "KVM 2",
		Category: "VPS",
		Prices: []hostingerCatalogPrice{{
			ID:               "hostingercom-vps-kvm2-usd-1m",
			Currency:         "USD",
			Price:            1799,
			FirstPeriodPrice: 899,
			Period:           1,
			PeriodUnit:       "month",
		}},
	}}, nil
}

func (f *fakeAPI) ListPaymentMethods(context.Context) ([]hostingerPaymentMethod, error) {
	if f.paymentMethods != nil {
		return append([]hostingerPaymentMethod(nil), f.paymentMethods...), nil
	}
	return []hostingerPaymentMethod{{ID: "7", Name: "Credit Card", PaymentMethod: "card", IsDefault: true}}, nil
}

func (f *fakeAPI) ListDataCenters(context.Context) ([]hostingerDataCenter, error) {
	if f.dataCenters != nil {
		return append([]hostingerDataCenter(nil), f.dataCenters...), nil
	}
	return []hostingerDataCenter{{ID: "3", Name: "EU"}}, nil
}

func (f *fakeAPI) ListTemplates(context.Context) ([]hostingerTemplate, error) {
	if f.templates != nil {
		return append([]hostingerTemplate(nil), f.templates...), nil
	}
	return []hostingerTemplate{{ID: "2", Name: "Ubuntu"}}, nil
}

func (f *fakeAPI) ListVMs(context.Context) ([]hostingerVM, error) {
	f.listCalls++
	return append([]hostingerVM(nil), f.vms...), nil
}

func (f *fakeAPI) GetVM(ctx context.Context, id string) (hostingerVM, error) {
	f.getCalls++
	if f.getWaitForContextAtCall > 0 && f.getCalls == f.getWaitForContextAtCall {
		<-ctx.Done()
		return hostingerVM{}, ctx.Err()
	}
	if f.getErrAtCall > 0 && f.getCalls == f.getErrAtCall {
		return hostingerVM{}, f.getErr
	}
	if len(f.getSequence) > 0 {
		vm := f.getSequence[0]
		f.getSequence = f.getSequence[1:]
		if vm.IDString() == id {
			return vm, nil
		}
	}
	for _, vm := range f.vms {
		if vm.IDString() == id {
			return vm, nil
		}
	}
	return hostingerVM{}, exit(4, "not found")
}

func (f *fakeAPI) PurchaseVM(_ context.Context, input hostingerPurchaseInput) (hostingerVM, error) {
	f.purchaseCalls++
	f.purchase = input
	if f.purchaseErrBeforeCreate != nil {
		return hostingerVM{}, f.purchaseErrBeforeCreate
	}
	vm := hostingerVM{ID: "vm-new", Hostname: input.Setup.Hostname, State: "running", IPv4: hostingerIPAddresses{"203.0.113.42"}}
	f.vms = append(f.vms, vm)
	if f.purchaseErrAfterCreate != nil {
		return hostingerVM{}, f.purchaseErrAfterCreate
	}
	return vm, nil
}

func (f *fakeAPI) SetupVM(_ context.Context, id string, input hostingerSetupInput) (hostingerVM, error) {
	f.setupCalls++
	f.setup = input
	for i := range f.vms {
		if f.vms[i].IDString() == id {
			f.vms[i].State = "running"
			f.vms[i].IPv4 = hostingerIPAddresses{"203.0.113.42"}
			return f.vms[i], nil
		}
	}
	return hostingerVM{}, exit(4, "not found")
}

func (f *fakeAPI) StartVM(_ context.Context, id string) error {
	f.startCalls++
	f.started = append(f.started, id)
	for i := range f.vms {
		if f.vms[i].IDString() == id {
			f.vms[i].State = "running"
			if f.vms[i].Host() == "" {
				f.vms[i].IPv4 = hostingerIPAddresses{"203.0.113.42"}
			}
			return nil
		}
	}
	return exit(4, "not found")
}

func (f *fakeAPI) StopVM(_ context.Context, id string) error {
	f.stopCalls++
	f.stopped = append(f.stopped, id)
	if f.stopErr != nil {
		return f.stopErr
	}
	if f.stopLeavesRunning {
		return nil
	}
	for i := range f.vms {
		if f.vms[i].IDString() == id {
			f.vms[i].State = "stopped"
		}
	}
	return nil
}
