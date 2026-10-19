package aws

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

// Exercise App.Run and the production AWS SDK adapter against a local EC2 query
// endpoint, including a name reassignment after DescribeKeyPairs returns.
func TestAWSStopKeyIdentityAtEC2Boundary(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		for _, scenario := range []string{"matching", "replacement", "reassigned", "legacy", "absent", "unmanaged"} {
			t.Run(fmt.Sprintf("fixed=%t/%s", fixed, scenario), func(t *testing.T) {
				testutil.IsolateUserDirs(t)
				repo := t.TempDir()
				t.Chdir(repo)
				cfg := fixedAWSTestConfig()
				cfg.AWSSSHCIDRs = []string{"127.0.0.1/32"}
				leaseID := "cbx_abcdef123484"
				server := awsTestServer("i-key-release", leaseID, "key-release", cfg.AWSRegion)
				server.Labels["provider_key"] = core.ProviderKeyForLease(leaseID)
				fake := &fakeAWSClient{}
				t.Cleanup(installFixedAWSTestClient(t, fake))
				if fixed {
					backend := NewAWSLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*awsLeaseBackend)
					lease, err := backend.Acquire(t.Context(), core.AcquireRequest{
						Repo: core.Repo{Root: repo}, Keep: true, RequestedLeaseID: leaseID, RequestedSlug: "key-release",
					})
					if err != nil {
						t.Fatal(err)
					}
					server = lease.Server
				} else {
					claimAWSCleanupServer(t, cfg, server)
				}
				claim, err := core.ReadLeaseClaim(leaseID)
				if err != nil {
					t.Fatal(err)
				}
				originalID := claim.Labels["aws_key_pair_id"]
				if scenario == "legacy" {
					if err := core.WithDurableLeaseClaimLock(leaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
						delete(c.Labels, "aws_key_pair_id")
						return persist()
					}); err != nil {
						t.Fatal(err)
					}
				}
				keyName := core.ServerProviderKey(server)
				currentID := originalID
				if scenario == "replacement" {
					currentID = "key-replacement"
				} else if scenario == "absent" {
					currentID = ""
				}
				var mu sync.Mutex
				var actions, deletedKeys []string
				terminations := 0
				endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					if err := r.ParseForm(); err != nil {
						t.Error(err)
						http.Error(w, "bad query", http.StatusBadRequest)
						return
					}
					action := r.Form.Get("Action")
					actions = append(actions, action)
					w.Header().Set("Content-Type", "text/xml")
					switch action {
					case "DescribeInstances":
						if terminations > 0 {
							fmt.Fprint(w, `<DescribeInstancesResponse><reservationSet/></DescribeInstancesResponse>`)
							return
						}
						fmt.Fprintf(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>%s</instanceId><instanceState><name>running</name></instanceState><tagSet>`, server.CloudID)
						for key, value := range server.Labels {
							fmt.Fprint(w, "<item><key>")
							_ = xml.EscapeText(w, []byte(key))
							fmt.Fprint(w, "</key><value>")
							_ = xml.EscapeText(w, []byte(value))
							fmt.Fprint(w, "</value></item>")
						}
						fmt.Fprint(w, `</tagSet></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`)
					case "GetCallerIdentity":
						fmt.Fprint(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`)
					case "TerminateInstances":
						if got := r.Form.Get("InstanceId.1"); got != server.CloudID {
							t.Errorf("terminated instance=%q, want %q", got, server.CloudID)
						}
						terminations++
						fmt.Fprint(w, `<TerminateInstancesResponse/>`)
					case "DescribeKeyPairs":
						if got := r.Form.Get("KeyName.1"); got != keyName {
							t.Errorf("looked up key=%q, want %q", got, keyName)
						}
						if currentID == "" {
							w.WriteHeader(http.StatusBadRequest)
							fmt.Fprint(w, `<Response><Errors><Error><Code>InvalidKeyPair.NotFound</Code></Error></Errors></Response>`)
							return
						}
						tagged := "true"
						if scenario == "unmanaged" {
							tagged = "false"
						}
						fmt.Fprintf(w, `<DescribeKeyPairsResponse><keySet><item><keyName>%s</keyName><keyPairId>%s</keyPairId><tagSet><item><key>crabbox</key><value>%s</value></item><item><key>created_by</key><value>crabbox</value></item></tagSet></item></keySet></DescribeKeyPairsResponse>`, keyName, currentID, tagged)
						if scenario == "reassigned" {
							currentID = "key-replacement"
						}
					case "DeleteKeyPair":
						id := r.Form.Get("KeyPairId")
						deletedKeys = append(deletedKeys, id)
						if r.Form.Get("KeyName") != "" || id != originalID {
							t.Errorf("unsafe key deletion: id=%q name=%q", id, r.Form.Get("KeyName"))
						}
						if id == currentID || r.Form.Get("KeyName") == keyName {
							currentID = ""
						} else {
							w.WriteHeader(http.StatusBadRequest)
							fmt.Fprint(w, `<Response><Errors><Error><Code>InvalidKeyPair.NotFound</Code></Error></Errors></Response>`)
							return
						}
						fmt.Fprint(w, `<DeleteKeyPairResponse><return>true</return></DeleteKeyPairResponse>`)
					default:
						t.Errorf("unexpected AWS action %q", action)
						http.Error(w, "unexpected action", http.StatusBadRequest)
					}
				}))
				defer endpoint.Close()
				// Synthetic credentials only; both AWS services are pinned to loopback.
				for key, value := range map[string]string{
					"AWS_ACCESS_KEY_ID": "test", "AWS_SECRET_ACCESS_KEY": "test", "AWS_SESSION_TOKEN": "",
					"AWS_PROFILE": "", "AWS_CONFIG_FILE": filepath.Join(repo, "no-aws-config"),
					"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(repo, "no-aws-credentials"),
					"AWS_ENDPOINT_URL_EC2":        endpoint.URL, "AWS_ENDPOINT_URL_STS": endpoint.URL,
					"AWS_EC2_METADATA_DISABLED": "true", "CRABBOX_AWS_REGION": cfg.AWSRegion,
					"CRABBOX_COORDINATOR": "", "CRABBOX_COORDINATOR_MODE": "", "CRABBOX_COORDINATOR_TOKEN_COMMAND": "",
					"CRABBOX_POND": "", "CRABBOX_TAILSCALE": "",
				} {
					t.Setenv(key, value)
				}
				configPath := filepath.Join(repo, "config.yaml")
				if err := os.WriteFile(configPath, []byte("provider: aws\ntarget: linux\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CRABBOX_CONFIG", configPath)
				newAWSClient = func(ctx context.Context, cfg core.Config) (awsClient, error) { return core.NewAWSClient(ctx, cfg) }
				var output bytes.Buffer
				stop := func() {
					t.Helper()
					if err := (core.App{Stdout: &output, Stderr: &output}).Run(t.Context(), []string{"stop", "--provider", "aws", "--id", leaseID}); err != nil {
						t.Fatalf("stop: %v; output=%s", err, output.String())
					}
				}
				stop()
				if scenario == "replacement" && !strings.Contains(output.String(), "does not match exact claim identity") {
					t.Errorf("missing replacement skip explanation: %s", output.String())
				}
				if scenario == "legacy" && !strings.Contains(output.String(), "no retained immutable key identity") {
					t.Errorf("missing legacy skip explanation: %s", output.String())
				}
				if scenario == "unmanaged" && !strings.Contains(output.String(), "lacks canonical Crabbox ownership tags") {
					t.Errorf("missing ownership skip explanation: %s", output.String())
				}
				if fixed && scenario != "legacy" {
					stop() // A terminal receipt must never repeat either provider deletion.
				}
				mu.Lock()
				defer mu.Unlock()
				if terminations != 1 {
					t.Fatalf("expected one instance termination: %v", actions)
				}
				wantDeletes := 0
				if scenario == "matching" || scenario == "reassigned" {
					wantDeletes = 1
				}
				if len(deletedKeys) != wantDeletes {
					t.Errorf("key deletions=%q, want %d", deletedKeys, wantDeletes)
				}
				if (scenario == "replacement" || scenario == "reassigned") && currentID != "key-replacement" {
					t.Errorf("replacement was deleted: current id=%q", currentID)
				}
				if scenario == "legacy" && slices.Contains(actions, "DescribeKeyPairs") {
					t.Error("looked up an unbound legacy key")
				}
				t.Logf("AWS actions=%v deleted_key_ids=%q remaining_key_id=%q", actions, deletedKeys, currentID)
			})
		}
	}
}
