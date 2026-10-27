package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

func TestAWSPublicIPAdaptiveCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var observed []time.Duration
		client := &AWSClient{ec2: ec2.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("fixture", "fixture", ""), HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			observed = append(observed, time.Since(start))
			ip := ""
			if len(observed) == 8 {
				ip = "203.0.113.44"
			}
			body := fmt.Sprintf(`<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>i-exact</instanceId><instanceState><name>running</name></instanceState><ipAddress>%s</ipAddress></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`, ip)
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})}})}
		server, err := client.WaitForServerIP(t.Context(), "i-exact")
		if err != nil || server.PublicNet.IPv4.IP == "" {
			t.Fatalf("IP wait: %+v %v", server, err)
		}
		want := []time.Duration{0, 250 * time.Millisecond, 750 * time.Millisecond, 1750 * time.Millisecond, 3750 * time.Millisecond, 7750 * time.Millisecond, 12750 * time.Millisecond, 17750 * time.Millisecond}
		for i, elapsed := range observed {
			if elapsed != want[i] {
				t.Fatalf("read %d at %s, want %s", i, elapsed, want[i])
			}
		}
	})
}

func TestNativeImageAvailabilityCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		reads := 0
		coord := &CoordinatorClient{BaseURL: "https://coordinator.test", Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			reads++
			state := "pending"
			if reads == 3 {
				state = "available"
			}
			body := fmt.Sprintf(`{"image":{"id":"ami-exact","state":%q}}`, state)
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})}}
		image, err := waitForImage(context.Background(), coord, "ami-exact", CoordinatorImageRef{}, time.Minute, io.Discard)
		if err != nil || image.ID != "ami-exact" || time.Since(start) != 6*time.Second {
			t.Fatalf("availability wait: %+v err=%v elapsed=%s", image, err, time.Since(start))
		}
	})
}
