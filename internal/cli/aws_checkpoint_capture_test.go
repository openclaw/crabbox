package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

type fakeAWSImageCaptureReader struct {
	images    func(context.Context, *ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error)
	snapshots func(context.Context, *ec2.DescribeSnapshotsInput) (*ec2.DescribeSnapshotsOutput, error)
}

func (f fakeAWSImageCaptureReader) DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	return f.images(ctx, in)
}
func (f fakeAWSImageCaptureReader) DescribeSnapshots(ctx context.Context, in *ec2.DescribeSnapshotsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
	return f.snapshots(ctx, in)
}

func TestAWSImageCaptureEstablishedBarrier(t *testing.T) {
	for _, scenario := range []string{"image not found", "empty image", "missing snapshot ID", "partial mapping", "snapshot not found", "missing snapshot", "snapshot error", "timeout", "in-flight timeout", "cancellation", "permission", "wrong image", "wrong owner", "wrong snapshot", "wrong volume", "image failed", "unknown snapshot state"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const imageID, account = "ami-exact", "123456789012"
				imageReads, snapshotReads := 0, 0
				start := time.Now()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				reader := fakeAWSImageCaptureReader{
					images: func(ctx context.Context, in *ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) {
						imageReads++
						if !slices.Equal(in.ImageIds, []string{imageID}) {
							t.Fatalf("wrong image IDs: %v", in.ImageIds)
						}
						if scenario == "in-flight timeout" {
							<-ctx.Done()
							return nil, ctx.Err()
						}
						if scenario == "cancellation" {
							cancel()
							return nil, ctx.Err()
						}
						if scenario == "permission" {
							return nil, &smithy.GenericAPIError{Code: "UnauthorizedOperation"}
						}
						if scenario == "image not found" && imageReads == 1 {
							return nil, &smithy.GenericAPIError{Code: "InvalidAMIID.NotFound"}
						}
						if scenario == "empty image" && imageReads == 1 {
							return &ec2.DescribeImagesOutput{}, nil
						}
						observed := types.Image{ImageId: aws.String(imageID), OwnerId: aws.String(account), State: types.ImageStatePending,
							BlockDeviceMappings: []types.BlockDeviceMapping{
								{DeviceName: aws.String("/dev/sda1"), Ebs: &types.EbsBlockDevice{SnapshotId: aws.String("snap-root")}},
								{DeviceName: aws.String("/dev/sdf"), Ebs: &types.EbsBlockDevice{SnapshotId: aws.String("snap-data")}},
							},
						}
						if scenario == "wrong image" {
							observed.ImageId = aws.String("ami-other")
						}
						if scenario == "wrong owner" {
							observed.OwnerId = aws.String("999999999999")
						}
						if scenario == "image failed" {
							observed.State = types.ImageStateFailed
						}
						if scenario == "timeout" || scenario == "missing snapshot ID" && imageReads == 1 {
							observed.BlockDeviceMappings[1].Ebs.SnapshotId = nil
						}
						if scenario == "partial mapping" && imageReads == 1 {
							observed.BlockDeviceMappings = observed.BlockDeviceMappings[:1]
						}
						return &ec2.DescribeImagesOutput{Images: []types.Image{observed}}, nil
					},
					snapshots: func(_ context.Context, in *ec2.DescribeSnapshotsInput) (*ec2.DescribeSnapshotsOutput, error) {
						snapshotReads++
						if !slices.Equal(in.SnapshotIds, []string{"snap-root", "snap-data"}) {
							t.Fatalf("wrong snapshot IDs: %v", in.SnapshotIds)
						}
						if scenario == "snapshot not found" && snapshotReads == 1 {
							return nil, &smithy.GenericAPIError{Code: "InvalidSnapshot.NotFound"}
						}
						out := &ec2.DescribeSnapshotsOutput{Snapshots: []types.Snapshot{
							{SnapshotId: aws.String("snap-root"), OwnerId: aws.String(account), VolumeId: aws.String("vol-root"), State: types.SnapshotStateCompleted},
							{SnapshotId: aws.String("snap-data"), OwnerId: aws.String(account), VolumeId: aws.String("vol-data"), State: types.SnapshotStatePending},
						}}
						if scenario == "snapshot error" {
							out.Snapshots[1].State = types.SnapshotStateError
						}
						if scenario == "wrong snapshot" {
							out.Snapshots[1].SnapshotId = aws.String("snap-other")
						}
						if scenario == "wrong volume" {
							out.Snapshots[1].VolumeId = aws.String("vol-other")
						}
						if scenario == "unknown snapshot state" {
							out.Snapshots[1].State = types.SnapshotState("recovering")
						}
						if scenario == "missing snapshot" && snapshotReads == 1 {
							out.Snapshots = out.Snapshots[:1]
						}
						return out, nil
					},
				}
				image := CoordinatorImage{ID: imageID, AccountID: account, State: "pending", Provider: "aws", Direct: true}
				result, err := waitForAWSImageCapture(ctx, reader, image, map[string]string{"/dev/sda1": "vol-root", "/dev/sdf": "vol-data"}, 2500*time.Millisecond)
				success := slices.Contains([]string{"image not found", "empty image", "missing snapshot ID", "partial mapping", "snapshot not found", "missing snapshot"}, scenario)
				if result.ID != imageID || result.AccountID != account {
					t.Fatalf("accepted identity lost: %+v", result)
				}
				if success {
					if err != nil || !result.captureEstablished || len(result.SnapshotIDs) != 2 || result.State != "pending" || time.Since(start) != time.Second {
						t.Fatalf("barrier=%+v err=%v elapsed=%s", result, err, time.Since(start))
					}
				} else {
					if result.captureEstablished {
						t.Fatal("uncertain AWS observation attests capture")
					}
					if err == nil || !strings.Contains(err.Error(), "uncertain") || strings.Contains(err.Error(), "established") {
						t.Fatalf("false capture attestation: %v", err)
					}
					if strings.Contains(scenario, "timeout") || scenario == "unknown snapshot state" {
						if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 2500*time.Millisecond {
							t.Fatalf("deadline not bounded: %v elapsed=%s", err, time.Since(start))
						}
					}
					if scenario == "cancellation" && !errors.Is(err, context.Canceled) {
						t.Fatalf("lost cancellation: %v", err)
					}
				}
			})
		})
	}
}
