package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	awsFixedAttemptTagSHA256      = "fixed_attempt_sha256"
	awsFixedAttemptTagRegion      = "fixed_attempt_region"
	awsFixedAttemptTagAZ          = "fixed_attempt_az"
	awsFixedAttemptTagSubnet      = "fixed_attempt_subnet"
	awsFixedAttemptTagType        = "fixed_attempt_type"
	awsFixedAttemptTagMarket      = "fixed_attempt_market"
	awsFixedAttemptTagImage       = "fixed_attempt_image"
	awsFixedAttemptTagSG          = "fixed_attempt_sg"
	awsFixedAttemptTagHost        = "fixed_attempt_host"
	awsFixedAttemptTagKeyPair     = "fixed_attempt_key_pair"
	awsFixedAttemptTagTokenSHA256 = "fixed_attempt_token_sha256"
)

type awsFixedAttemptAttestation struct {
	Region            string `json:"region"`
	AvailabilityZone  string `json:"availabilityZone"`
	SubnetID          string `json:"subnetID"`
	ServerType        string `json:"serverType"`
	Market            string `json:"market"`
	ImageID           string `json:"imageID"`
	SecurityGroupID   string `json:"securityGroupID"`
	HostID            string `json:"hostID"`
	KeyPairID         string `json:"keyPairID"`
	ClientTokenSHA256 string `json:"clientTokenSHA256"`
}

func AWSFixedAttemptAttestationLabels(attempt AWSLaunchAttempt) map[string]string {
	attestation := awsFixedAttemptAttestationFor(attempt)
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"crabbox-fixed-aws-attempt-attestation-v1",
		attestation.Region,
		attestation.AvailabilityZone,
		attestation.SubnetID,
		attestation.ServerType,
		attestation.Market,
		attestation.ImageID,
		attestation.SecurityGroupID,
		attestation.HostID,
		attestation.KeyPairID,
		attestation.ClientTokenSHA256,
	}, "\x00")))
	return map[string]string{
		awsFixedAttemptTagSHA256:      hex.EncodeToString(digest[:]),
		awsFixedAttemptTagRegion:      attestation.Region,
		awsFixedAttemptTagAZ:          attestation.AvailabilityZone,
		awsFixedAttemptTagSubnet:      attestation.SubnetID,
		awsFixedAttemptTagType:        attestation.ServerType,
		awsFixedAttemptTagMarket:      attestation.Market,
		awsFixedAttemptTagImage:       attestation.ImageID,
		awsFixedAttemptTagSG:          attestation.SecurityGroupID,
		awsFixedAttemptTagHost:        attestation.HostID,
		awsFixedAttemptTagKeyPair:     attestation.KeyPairID,
		awsFixedAttemptTagTokenSHA256: attestation.ClientTokenSHA256,
	}
}

func ValidateAWSFixedAttemptAttestation(labels map[string]string, attempt AWSLaunchAttempt) error {
	expected := AWSFixedAttemptAttestationLabels(attempt)
	for key, expectedValue := range expected {
		actualValue, ok := labels[key]
		if !ok {
			return fmt.Errorf("missing AWS fixed attempt tag %s", key)
		}
		if actualValue != expectedValue {
			return fmt.Errorf("AWS fixed attempt tag %s=%q does not match %q", key, actualValue, expectedValue)
		}
	}
	return nil
}

func awsFixedAttemptAttestationFor(attempt AWSLaunchAttempt) awsFixedAttemptAttestation {
	clientTokenDigest := sha256.Sum256([]byte(strings.TrimSpace(attempt.ClientToken)))
	return awsFixedAttemptAttestation{
		Region:            strings.TrimSpace(attempt.Region),
		AvailabilityZone:  strings.TrimSpace(attempt.AvailabilityZone),
		SubnetID:          strings.TrimSpace(attempt.SubnetID),
		ServerType:        strings.TrimSpace(attempt.ServerType),
		Market:            strings.TrimSpace(attempt.Market),
		ImageID:           strings.TrimSpace(attempt.ImageID),
		SecurityGroupID:   strings.TrimSpace(attempt.SecurityGroupID),
		HostID:            strings.TrimSpace(attempt.HostID),
		KeyPairID:         strings.TrimSpace(attempt.KeyPairID),
		ClientTokenSHA256: hex.EncodeToString(clientTokenDigest[:]),
	}
}
