package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestAWSWaitForServerIPStopsOnTerminalState(t *testing.T) {
	for _, tc := range []struct{ name, state, reason, spot, ip, want string }{
		{name: "spot reclaimed mid-wait", state: "shutting-down", reason: "Server.SpotInstanceTermination", want: "Server.SpotInstanceTermination"},
		{name: "terminated mid-wait", state: "terminated", reason: "Client.UserInitiatedShutdown", want: "Client.UserInitiatedShutdown"},
		{name: "closed spot request before instance state converges", state: "pending", spot: "sir-test", want: "instance-terminated-no-capacity"},
		{name: "terminal with stale address", state: "terminated", ip: "203.0.113.44", want: "terminated"},
		{name: "healthy slow boot", state: "running", ip: "203.0.113.44"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			describes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
					return
				}
				switch r.Form.Get("Action") {
				case "DescribeInstances":
					describes++
					state, reason, ip := "pending", "", ""
					if describes > 1 {
						state, reason, ip = tc.state, tc.reason, tc.ip
					}
					writeEC2XML(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>i-test</instanceId><instanceType>t3.small</instanceType><instanceState><name>`+state+`</name></instanceState><stateReason><code>`+reason+`</code></stateReason><spotInstanceRequestId>`+tc.spot+`</spotInstanceRequestId><ipAddress>`+ip+`</ipAddress></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`)
				case "DescribeSpotInstanceRequests":
					if r.Form.Get("SpotInstanceRequestId.1") != "sir-test" {
						t.Errorf("wrong Spot request: %v", r.Form)
					}
					state, code := "active", "fulfilled"
					if describes > 1 {
						state, code = "closed", "instance-terminated-no-capacity"
					}
					writeEC2XML(w, `<DescribeSpotInstanceRequestsResponse><spotInstanceRequestSet><item><spotInstanceRequestId>sir-test</spotInstanceRequestId><instanceId>i-test</instanceId><state>`+state+`</state><status><code>`+code+`</code><message>Spot capacity reclaimed</message></status></item></spotInstanceRequestSet></DescribeSpotInstanceRequestsResponse>`)
				default:
					t.Errorf("unexpected EC2 action: %v", r.Form)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			got, err := testAWSClient(server.URL).WaitForServerIP(ctx, "i-test")
			if tc.want == "" {
				if err != nil || got.PublicNet.IPv4.IP != tc.ip {
					t.Fatalf("healthy boot: server=%+v err=%v", got, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "i-test") {
				t.Fatalf("error=%v, want instance ID and %q", err, tc.want)
			}
			if describes != 2 {
				t.Fatalf("describes=%d, want 2", describes)
			}
		})
	}
}

func TestAWSTerminalStateOverridesReadyTag(t *testing.T) {
	for _, state := range []string{"shutting-down", "terminated"} {
		server := awsInstanceToServer(types.Instance{
			InstanceId: aws.String("i-test"), State: &types.InstanceState{Name: types.InstanceStateName(state)},
			Tags: []types.Tag{{Key: aws.String("state"), Value: aws.String("ready")}},
		})
		if server.Labels["state"] != state {
			t.Fatalf("state=%s ready tag survived: %+v", state, server)
		}
		cfg := defaultConfig()
		cfg.Provider, cfg.Network = "aws", NetworkPublic
		view, err := statusViewFromLeaseTarget(t.Context(), cfg, LeaseTarget{Server: server, LeaseID: "cbx_status"})
		if err != nil || view.State != state || view.Ready {
			t.Fatalf("terminal status view=%+v err=%v", view, err)
		}
	}
}
