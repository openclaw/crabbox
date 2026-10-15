package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const awsImageCapturePollInterval = time.Second

func (c *AWSClient) imageCheckpointSourceDevices(ctx context.Context, instanceID string) (map[string]string, error) {
	out, err := c.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return nil, err
	}
	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			if aws.ToString(instance.InstanceId) != instanceID {
				continue
			}
			devices := make(map[string]string)
			for _, mapping := range instance.BlockDeviceMappings {
				if mapping.Ebs == nil {
					continue
				}
				device, volume := aws.ToString(mapping.DeviceName), aws.ToString(mapping.Ebs.VolumeId)
				if device == "" || volume == "" {
					return nil, Exit(5, "AWS checkpoint source has an incomplete EBS mapping")
				}
				devices[device] = volume
			}
			if len(devices) == 0 {
				return nil, Exit(5, "AWS checkpoint source has no EBS devices to capture")
			}
			return devices, nil
		}
	}
	return nil, Exit(4, "aws instance not found: %s", instanceID)
}

type awsImageCaptureReader interface {
	DescribeImages(context.Context, *ec2.DescribeImagesInput, ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	DescribeSnapshots(context.Context, *ec2.DescribeSnapshotsInput, ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error)
}

func (c *AWSClient) waitForImageCapture(ctx context.Context, image CoordinatorImage, devices map[string]string, timeout time.Duration) (CoordinatorImage, error) {
	return waitForAWSImageCapture(ctx, c.ec2, image, devices, timeout)
}

func waitForAWSImageCapture(ctx context.Context, reader awsImageCaptureReader, image CoordinatorImage, devices map[string]string, timeout time.Duration) (result CoordinatorImage, err error) {
	result = image
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if err != nil {
			err = fmt.Errorf("capture of AWS image %s is uncertain; retain its exact identity and inspect the checkpoint before continuing source writes: %w", image.ID, err)
			err = errors.Join(Exit(5, "%s", err), err)
		}
	}()
	if image.ID == "" || image.AccountID == "" || len(devices) == 0 {
		return result, Exit(5, "capture observation requires exact image, account, and source EBS identities")
	}
	for {
		if err := waitCtx.Err(); err != nil {
			return result, err
		}
		out, readErr := reader.DescribeImages(waitCtx, &ec2.DescribeImagesInput{ImageIds: []string{image.ID}})
		if readErr != nil && awsAPIErrorCode(readErr) != "InvalidAMIID.NotFound" {
			return result, readErr
		}
		if readErr == nil && len(out.Images) > 0 {
			if len(out.Images) != 1 || aws.ToString(out.Images[0].ImageId) != image.ID || aws.ToString(out.Images[0].OwnerId) != image.AccountID {
				return result, Exit(5, "AWS image identity or owner changed during capture observation")
			}
			observed := out.Images[0]
			result.State = string(observed.State)
			result.SnapshotIDs = awsImageSnapshotIDs(observed)
			result.Architecture = string(observed.Architecture)
			if observed.State == types.ImageStateFailed || observed.State == types.ImageStateInvalid || observed.State == types.ImageStateDeregistered {
				return result, Exit(5, "AWS image %s failed in state %s", image.ID, observed.State)
			}
			complete, observeErr := awsImageSnapshotsEstablished(waitCtx, reader, observed, devices, image.AccountID)
			if observeErr != nil {
				return result, observeErr
			}
			if complete {
				if err := waitCtx.Err(); err != nil {
					return result, err
				}
				result.captureEstablished = true
				return result, nil
			}
		}
		if err := sleepContext(waitCtx, awsImageCapturePollInterval); err != nil {
			return result, err
		}
	}
}

func awsImageSnapshotsEstablished(ctx context.Context, reader awsImageCaptureReader, image types.Image, devices map[string]string, accountID string) (bool, error) {
	snapshotVolumes := make(map[string]string)
	seenDevices := make(map[string]bool)
	for _, mapping := range image.BlockDeviceMappings {
		if mapping.Ebs == nil {
			continue
		}
		device, snapshotID := aws.ToString(mapping.DeviceName), aws.ToString(mapping.Ebs.SnapshotId)
		volumeID := devices[device]
		if volumeID == "" || seenDevices[device] {
			return false, Exit(5, "AWS image EBS device mapping differs from the checkpoint source")
		}
		seenDevices[device] = true
		if snapshotID == "" {
			return false, nil
		}
		if _, exists := snapshotVolumes[snapshotID]; exists {
			return false, Exit(5, "AWS image reuses a backing snapshot for multiple source devices")
		}
		snapshotVolumes[snapshotID] = volumeID
	}
	// DescribeImages can publish only part of the mapping while CreateImage is
	// initiating snapshots. Every source EBS device must have its own snapshot.
	if len(seenDevices) != len(devices) {
		return false, nil
	}
	out, err := reader.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{SnapshotIds: awsImageSnapshotIDs(image)})
	if err != nil {
		if awsAPIErrorCode(err) == "InvalidSnapshot.NotFound" {
			return false, nil
		}
		return false, err
	}
	seenSnapshots := make(map[string]bool)
	for _, snapshot := range out.Snapshots {
		id := aws.ToString(snapshot.SnapshotId)
		if snapshotVolumes[id] == "" || seenSnapshots[id] || aws.ToString(snapshot.OwnerId) != accountID || aws.ToString(snapshot.VolumeId) != snapshotVolumes[id] {
			return false, Exit(5, "AWS backing snapshot identity, owner, or source volume changed")
		}
		seenSnapshots[id] = true
		switch snapshot.State {
		case types.SnapshotStatePending, types.SnapshotStateCompleted:
		case types.SnapshotStateError:
			return false, Exit(5, "AWS backing snapshot %s failed in state error", id)
		default:
			return false, nil
		}
	}
	return len(seenSnapshots) == len(snapshotVolumes), nil
}
