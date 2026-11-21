package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// The real SDK serializes identity, inventory and termination requests. The
// endpoint deliberately ignores filters, proving local exclusion at the effect.
func TestAWSSMSessionCleanupSDKAuthority(t *testing.T) {
	for _, wrongAccount := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign principal and target", true: "inconsistent caller account"}[wrongAccount], func(t *testing.T) {
			var terminated []string
			closed := false
			identityReads := 0
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Header.Get("X-Amz-Target") {
				case "AmazonSSM.DescribeSessions":
					w.Header().Set("Content-Type", "application/x-amz-json-1.1")
					if closed {
						_, _ = w.Write([]byte(`{"Sessions":[]}`))
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"Sessions": []map[string]string{
						{"SessionId": "owned", "Target": testSSMTarget, "Owner": testSSMOwner},
						{"SessionId": "foreign-principal", "Target": testSSMTarget, "Owner": strings.Replace(testSSMOwner, "session-a", "session-b", 1)},
						{"SessionId": "foreign-target", "Target": "i-fedcba98765432100", "Owner": testSSMOwner},
					}})
				case "AmazonSSM.TerminateSession":
					var req struct {
						SessionID string `json:"SessionId"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					terminated = append(terminated, req.SessionID)
					closed = true
					w.Header().Set("Content-Type", "application/x-amz-json-1.1")
					_, _ = w.Write([]byte(`{}`))
				default:
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					if r.Form.Get("Action") != "GetCallerIdentity" {
						t.Errorf("unexpected request %s", r.Form)
						w.WriteHeader(400)
						return
					}
					identityReads++
					account := "123456789012"
					if wrongAccount {
						account = "999999999999"
					}
					writeSTSXML(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Account>`+account+`</Account><Arn>`+testSSMOwner+`</Arn><UserId>fixture</UserId></GetCallerIdentityResult></GetCallerIdentityResponse>`)
				}
			}))
			defer endpoint.Close()
			cfg := aws.Config{Region: "us-west-2", Credentials: credentials.NewStaticCredentialsProvider("fixture", "fixture", ""), BaseEndpoint: aws.String(endpoint.URL)}
			client := &AWSClient{sts: sts.NewFromConfig(cfg), ssm: ssm.NewFromConfig(cfg)}
			err := client.TerminateOwnedSSMSessions(t.Context(), testSSMTarget)
			if identityReads != 1 || (err != nil) != wrongAccount {
				t.Fatalf("identity=%d err=%v", identityReads, err)
			}
			if wrongAccount {
				if len(terminated) != 0 {
					t.Fatalf("unauthorized effects=%v", terminated)
				}
			} else if !slices.Equal(terminated, []string{"owned"}) {
				t.Fatalf("effects=%v", terminated)
			}
		})
	}
}
