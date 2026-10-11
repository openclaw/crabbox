package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// AWSAcquireStateError reports an instance that cannot finish acquiring.
type AWSAcquireStateError struct {
	InstanceID string
	State      string
	Reason     string
}

func (e *AWSAcquireStateError) Error() string {
	return fmt.Sprintf("AWS instance %s cannot finish acquiring: state=%s; %s", e.InstanceID, e.State, e.Reason)
}

func (c *AWSClient) CheckAcquireState(ctx context.Context, id string) error {
	_, err := c.getServerForAcquire(ctx, id)
	return err
}

func (c *AWSClient) getServerForAcquire(ctx context.Context, id string) (Server, error) {
	server, err := c.GetServer(ctx, id)
	if err != nil {
		return Server{}, err
	}
	terminal := server.Status == "shutting-down" || server.Status == "terminated"
	reason, _ := server.ProviderMetadata["stateReasonCode"].(string)
	message, _ := server.ProviderMetadata["stateReasonMessage"].(string)
	requestID, _ := server.ProviderMetadata["spotInstanceRequestID"].(string)
	if requestID != "" {
		out, spotErr := c.ec2.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{SpotInstanceRequestIds: []string{requestID}})
		// Spot diagnostics are best effort: older IAM policies may only permit
		// DescribeInstances. Never lose an already-observed terminal state.
		if spotErr == nil {
			for _, request := range out.SpotInstanceRequests {
				if aws.ToString(request.SpotInstanceRequestId) != requestID || aws.ToString(request.InstanceId) != id || request.Status == nil {
					continue
				}
				code := aws.ToString(request.Status.Code)
				if request.State == types.SpotInstanceStateClosed && strings.HasPrefix(code, "instance-terminated-") {
					terminal = true
					reason, message = code, aws.ToString(request.Status.Message)
				}
			}
		}
	}
	if terminal {
		return Server{}, &AWSAcquireStateError{InstanceID: id, State: server.Status, Reason: strings.TrimSpace(reason + " " + message)}
	}
	return server, nil
}
