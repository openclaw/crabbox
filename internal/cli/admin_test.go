package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminMacHostsRequiresForceForAllocate(t *testing.T) {
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.adminMacHosts(context.Background(), []string{"allocate", "--availability-zone", "eu-west-1a"})
	if err == nil || !strings.Contains(err.Error(), "requires --force") {
		t.Fatalf("err=%v, want force requirement", err)
	}
}

func TestAdminMacHostsRequiresForceForRelease(t *testing.T) {
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.adminMacHosts(context.Background(), []string{"release", "h-000000000001"})
	if err == nil || !strings.Contains(err.Error(), "requires --force") {
		t.Fatalf("err=%v, want force requirement", err)
	}
}

func TestAdminMacHostsReleaseAcceptsFlagsAfterHostID(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var gotPath, gotRegion, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRegion = r.URL.Query().Get("region")
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodDelete {
			t.Fatalf("method=%s, want DELETE", r.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"released": []string{"h-000000000001"}})
	}))
	defer server.Close()

	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "admin-token")
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	if err := app.adminHosts(context.Background(), []string{"release", "h-000000000001", "--provider", "aws", "--target", "macos", "--region", "us-east-1", "--force"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/admin/hosts/h-000000000001" || gotRegion != "us-east-1" {
		t.Fatalf("release request path=%q region=%q, want host route in us-east-1", gotPath, gotRegion)
	}
	if gotAuth != "Bearer admin-token" {
		t.Fatalf("auth=%q, want admin token", gotAuth)
	}
}

func TestAdminMacHostsRejectsMissingSubcommand(t *testing.T) {
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	err := app.adminMacHosts(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "usage: crabbox admin mac-hosts") {
		t.Fatalf("err=%v, want usage error", err)
	}
}

func TestAdminHostsRejectsUnsupportedScope(t *testing.T) {
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.adminHosts(context.Background(), []string{"policy", "--provider", "azure", "--target", "macos"})
	if err == nil || !strings.Contains(err.Error(), "currently supports --provider aws --target macos") {
		t.Fatalf("err=%v, want unsupported scope", err)
	}
}

func TestAdminMacHostsPolicyPrintsLifecyclePermissions(t *testing.T) {
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.adminMacHosts(context.Background(), []string{"policy"}); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{
		`"ec2:DescribeInstanceTypeOfferings"`,
		`"ec2:DescribeHosts"`,
		`"ec2:AllocateHosts"`,
		`"ec2:ReleaseHosts"`,
		`"ec2:CreateTags"`,
		`"ec2:CreateAction": "AllocateHosts"`,
		`"servicequotas:GetServiceQuota"`,
		`"servicequotas:ListServiceQuotas"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("policy missing %s:\n%s", want, out)
		}
	}
}

func TestAdminHostsPolicyPrintsLifecyclePermissions(t *testing.T) {
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.adminHosts(context.Background(), []string{"policy", "--provider", "aws", "--target", "macos"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"ec2:AllocateHosts"`) {
		t.Fatalf("policy missing host lifecycle permission:\n%s", stdout.String())
	}
}

func TestAdminAWSPolicyPrintsProviderPermissions(t *testing.T) {
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.adminAWSPolicy(nil); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{
		`"ec2:RunInstances"`,
		`"ec2:TerminateInstances"`,
		`"ec2:CreateSecurityGroup"`,
		`"ec2:CreateImage"`,
		`"ec2:RegisterImage"`,
		`"ec2:DeleteSnapshot"`,
		`"servicequotas:GetServiceQuota"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("policy missing %s:\n%s", want, out)
		}
	}
}

func TestAdminAWSPolicyCanIncludeMacHostPermissions(t *testing.T) {
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.adminAWSPolicy([]string{"--mac-hosts"}); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{
		`"ec2:RunInstances"`,
		`"ec2:AllocateHosts"`,
		`"ec2:ReleaseHosts"`,
		`"ec2:CreateAction": "AllocateHosts"`,
		`"servicequotas:GetServiceQuota"`,
		`"servicequotas:ListServiceQuotas"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("combined policy missing %s:\n%s", want, out)
		}
	}
	var doc iamPolicyDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("combined policy is invalid JSON: %v\n%s", err, out)
	}
	if len(doc.Statement) < 6 {
		t.Fatalf("combined policy statements=%d, want provider plus mac-host statements", len(doc.Statement))
	}
}

func TestAdminProvidersPolicyCanSelectMacOSHostPermissions(t *testing.T) {
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.adminProviders(context.Background(), []string{"policy", "--provider", "aws", "--target", "macos"}); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	for _, want := range []string{`"ec2:RunInstances"`, `"ec2:AllocateHosts"`, `"servicequotas:ListServiceQuotas"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("combined provider policy missing %s:\n%s", want, out)
		}
	}
}

func TestSummarizeMacHostDryRunMessage(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "dry run",
			message: "<Error><Code>DryRunOperation</Code><Message>Request would have succeeded</Message></Error>",
			want:    "DryRunOperation: request would have succeeded",
		},
		{
			name:    "unauthorized",
			message: "<Error><Code>UnauthorizedOperation</Code><Message>provider authorization details omitted</Message></Error>",
			want:    "UnauthorizedOperation: coordinator AWS identity needs EC2 Mac host lifecycle permissions, including ec2:AllocateHosts and ec2:CreateTags",
		},
		{
			name:    "other aws code",
			message: "<Error><Code>HostLimitExceeded</Code><Message>limit exceeded</Message></Error>",
			want:    "HostLimitExceeded",
		},
		{
			name:    "blank",
			message: "",
			want:    "-",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summarizeMacHostDryRunMessage(tt.message); got != tt.want {
				t.Fatalf("summary=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeMacHostDryRunChecks(t *testing.T) {
	checks := sanitizeMacHostDryRunChecks([]CoordinatorMacHostAllocationDryRun{
		{
			Region:           "eu-west-1",
			AvailabilityZone: "eu-west-1b",
			InstanceType:     "mac2.metal",
			Message:          `<Error><Code>UnauthorizedOperation</Code><Message>User: arn:aws:iam::123456789012:user/example is not authorized. Encoded authorization failure message: secret</Message></Error>`,
		},
	})
	if len(checks) != 1 {
		t.Fatalf("checks=%#v", checks)
	}
	got := checks[0].Message
	if !strings.Contains(got, "UnauthorizedOperation: coordinator AWS identity needs EC2 Mac host lifecycle permissions") {
		t.Fatalf("message=%q", got)
	}
	if strings.Contains(got, "123456789012") || strings.Contains(got, "Encoded authorization") {
		t.Fatalf("message leaked provider details: %q", got)
	}
}
