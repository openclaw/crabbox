package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestAWSProviderLifetimePolicyScopesDestructivePermissions(t *testing.T) {
	for _, extra := range [][]string{nil, {"--mac-hosts"}, {"--target", "macos"}} {
		t.Run(strings.Join(extra, "_"), func(t *testing.T) {
			var stdout bytes.Buffer
			app := App{Stdout: &stdout, Stderr: io.Discard}
			args := append([]string{"--provider", "aws", "--region", "eu-west-1", "--account", "123456789012"}, extra...)
			if err := app.adminProviderPolicy("admin providers policy", "", args); err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Statement []struct {
					Action    json.RawMessage
					Resource  string
					Condition map[string]map[string]string
				}
			}
			if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			for _, statement := range doc.Statement {
				var actions []string
				if err := json.Unmarshal(statement.Action, &actions); err != nil {
					var action string
					if err := json.Unmarshal(statement.Action, &action); err != nil {
						t.Fatal(err)
					}
					actions = []string{action}
				}
				for _, action := range actions {
					counts[action]++
					if action == "ec2:DescribeVolumes" && statement.Condition["StringEquals"]["aws:RequestedRegion"] != "eu-west-1" {
						t.Fatal("DescribeVolumes must use the selected Region")
					}
					if action != "ec2:StopInstances" && action != "ec2:TerminateInstances" {
						continue
					}
					if statement.Resource != "arn:aws:ec2:eu-west-1:123456789012:instance/*" {
						t.Fatalf("%s resource=%q", action, statement.Resource)
					}
					for key, value := range map[string]string{
						"aws:RequestedRegion":        "eu-west-1",
						"aws:ResourceAccount":        "123456789012",
						"ec2:ResourceTag/crabbox":    "true",
						"ec2:ResourceTag/created_by": "crabbox",
					} {
						if got := statement.Condition["StringEquals"][key]; got != value {
							t.Fatalf("%s condition %s=%q, want %q", action, key, got, value)
						}
					}
				}
			}
			for _, action := range []string{"ec2:StopInstances", "ec2:TerminateInstances", "ec2:DescribeVolumes"} {
				if counts[action] != 1 {
					t.Fatalf("%s grants=%d, want exactly one", action, counts[action])
				}
			}
		})
	}
}

func TestAWSProviderLifetimePolicyDefaultsToPrincipalAccountAndRegionTemplate(t *testing.T) {
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.adminAWSPolicy(nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`arn:aws:ec2:<aws-region>:*:instance/*`,
		`"aws:RequestedRegion": "<aws-region>"`,
		`"aws:ResourceAccount": "${aws:PrincipalAccount}"`,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("missing safe template %q: %s", want, stdout.String())
		}
	}
}

func TestAWSProviderLifetimePolicyRejectsUnsafeScope(t *testing.T) {
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	for _, args := range [][]string{
		{"--account", "*"}, {"--account", "12345678901"},
		{"--region", "*"}, {"--region", "eu-west-1:*"},
	} {
		if err := app.adminAWSPolicy(args); err == nil {
			t.Fatalf("accepted unsafe policy scope %q", args)
		}
	}
}
