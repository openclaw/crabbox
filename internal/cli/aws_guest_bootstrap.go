package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"golang.org/x/crypto/ssh"
)

type awsGuestBootstrapAPI interface {
	DescribeInstanceAttribute(context.Context, *ec2.DescribeInstanceAttributeInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceAttributeOutput, error)
	GetConsoleOutput(context.Context, *ec2.GetConsoleOutputInput, ...func(*ec2.Options)) (*ec2.GetConsoleOutputOutput, error)
}

func guestBootstrapHash(encoded string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", errors.New("invalid public-key bootstrap encoding")
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
func observedGuestHostKey(output string) (string, error) {
	const marker = "CRABBOX_GUEST_HOST_KEY_V1 "
	var found string
	for _, line := range strings.Split(output, "\n") {
		at := strings.Index(line, marker)
		if at < 0 {
			continue
		}
		fields := strings.Fields(line[at+len(marker):])
		if len(fields) < 2 || fields[0] != "ssh-ed25519" {
			return "", errors.New("guest console public host key invalid")
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.Join(fields[:2], " ")))
		if err != nil {
			return "", errors.New("guest console public host key invalid")
		}
		value := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if found != "" && found != value {
			return "", errors.New("guest console public host key ambiguous")
		}
		found = value
	}
	if found == "" {
		return "", errors.New("guest public host key pending")
	}
	return found, nil
}
func observeAWSGuestBootstrap(ctx context.Context, api AWSGuestNetworkAPI, b AWSGuestNetworkBoundary, policy GuestNetworkPolicy, instance types.Instance) (map[string]string, error) {
	facts := map[string]string{"imds": "disabled"}
	if policy.BootstrapMode == "" {
		if instance.MetadataOptions.HttpEndpoint != types.InstanceMetadataEndpointStateDisabled {
			return nil, errors.New("guest metadata endpoint policy mismatch")
		}
		return facts, nil
	}
	meta := instance.MetadataOptions
	if policy.BootstrapMode != GuestBootstrapPublicKeyV1 || b.BootstrapMode != policy.BootstrapMode || len(b.BootstrapSHA256) != 64 || meta.HttpEndpoint != types.InstanceMetadataEndpointStateEnabled || meta.HttpTokens != types.HttpTokensStateRequired || aws.ToInt32(meta.HttpPutResponseHopLimit) != 1 || meta.InstanceMetadataTags != types.InstanceMetadataTagsStateDisabled || meta.HttpProtocolIpv6 != types.InstanceMetadataProtocolStateDisabled {
		return nil, errors.New("guest roleless metadata/bootstrap policy mismatch")
	}
	control, ok := api.(awsGuestBootstrapAPI)
	if !ok {
		return nil, errors.New("provider cannot inspect guest bootstrap control-plane facts")
	}
	attribute, err := control.DescribeInstanceAttribute(ctx, &ec2.DescribeInstanceAttributeInput{InstanceId: instance.InstanceId, Attribute: types.InstanceAttributeNameUserData})
	if err != nil {
		return nil, err
	}
	if attribute == nil || aws.ToString(attribute.InstanceId) != aws.ToString(instance.InstanceId) || attribute.UserData == nil {
		return nil, errors.New("guest bootstrap resource observation mismatch")
	}
	hash, err := guestBootstrapHash(aws.ToString(attribute.UserData.Value))
	if err != nil || hash != b.BootstrapSHA256 {
		return nil, errors.New("guest public-key bootstrap content mismatch")
	}
	pin := ""
	for _, tag := range instance.Tags {
		if aws.ToString(tag.Key) == "guest_network_ssh_host_key" {
			if pin != "" {
				return nil, errors.New("guest host-key pin ambiguous")
			}
			pin = aws.ToString(tag.Value)
		}
	}
	if pin == "" {
		output, err := control.GetConsoleOutput(ctx, &ec2.GetConsoleOutputInput{InstanceId: instance.InstanceId, Latest: aws.Bool(true)})
		if err != nil {
			return nil, err
		}
		if output == nil || aws.ToString(output.InstanceId) != aws.ToString(instance.InstanceId) {
			return nil, errors.New("guest console resource mismatch")
		}
		data, err := base64.StdEncoding.DecodeString(aws.ToString(output.Output))
		if err != nil {
			return nil, errors.New("guest console observation invalid")
		}
		pin, err = observedGuestHostKey(string(data))
		if err != nil {
			return nil, err
		}
	}
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(pin))
	if err != nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("guest provider host-key pin invalid")
	}
	facts["imds"] = "roleless-v2"
	facts["bootstrapMode"] = policy.BootstrapMode
	facts["bootstrapSha256"] = hash
	facts["sshHostKey"] = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	return facts, nil
}
func (c *AWSClient) FinalizeGuestNetworkBootstrap(ctx context.Context, cfg Config, server *Server, leaseID string) error {
	if cfg.GuestNetworkPolicy == nil || cfg.GuestNetworkPolicy.BootstrapMode != GuestBootstrapPublicKeyV1 {
		return nil
	}
	b, err := AWSGuestNetworkBoundaryFromServer(*server)
	if err != nil {
		return err
	}
	if b.LeaseID != leaseID {
		return errors.New("guest bootstrap lease mismatch")
	}
	var receipt GuestNetworkReceipt
	for attempt := 0; attempt < 60; attempt++ {
		receipt, err = c.InspectGuestNetwork(ctx, b, server.CloudID, *cfg.GuestNetworkPolicy)
		if err == nil {
			break
		}
		if err.Error() != "guest public host key pending" {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if err != nil {
		return err
	}
	key := receipt.Facts["sshHostKey"]
	if key == "" {
		return errors.New("guest public host-key observation missing")
	}
	if old := server.Labels["guest_network_ssh_host_key"]; old != "" {
		if old != key {
			return errors.New("guest host-key pin changed")
		}
		return nil
	}
	if err := c.SetTags(ctx, server.CloudID, map[string]string{"guest_network_ssh_host_key": key}); err != nil {
		return fmt.Errorf("persist provider guest host-key pin: %w", err)
	}
	server.Labels["guest_network_ssh_host_key"] = key
	return nil
}
