package aws

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const (
	awsCacheVolumeABI         = "ext4-v1"
	awsCacheVolumeMaxMembers  = 8
	awsCacheVolumeMaxBindings = 11
	awsCacheVolumeStateFile   = "cache-volumes-v1.json"
	awsCacheVolumeGCMinAge    = 7 * 24 * time.Hour
)

type awsCacheVolumeLifecycle struct{}

type awsCacheVolumeRegistry struct {
	SchemaVersion  int                    `json:"schemaVersion"`
	InstallationID string                 `json:"installationID"`
	Records        []awsCacheVolumeRecord `json:"records"`
}

type awsCacheVolumeRecord struct {
	CacheSetID       string                   `json:"cacheSetID"`
	MemberID         string                   `json:"memberID"`
	TenantDigest     string                   `json:"tenantDigest"`
	RepoScopeDigest  string                   `json:"repoScopeDigest"`
	KeyDigest        string                   `json:"keyDigest"`
	ABIDigest        string                   `json:"abiDigest"`
	Name             string                   `json:"name"`
	Path             string                   `json:"path"`
	SizeGB           int                      `json:"sizeGB"`
	Region           string                   `json:"region"`
	AvailabilityZone string                   `json:"availabilityZone"`
	VolumeID         string                   `json:"volumeID,omitempty"`
	Generation       int64                    `json:"generation"`
	State            core.AWSCacheVolumeState `json:"state"`
	LeaseID          string                   `json:"leaseID,omitempty"`
	InstanceID       string                   `json:"instanceID,omitempty"`
	PurgeOnRelease   bool                     `json:"purgeOnRelease,omitempty"`
	LastError        string                   `json:"lastError,omitempty"`
	LastErrorAt      string                   `json:"lastErrorAt,omitempty"`
	RetryCount       int                      `json:"retryCount,omitempty"`
	CreatedAt        string                   `json:"createdAt"`
	UpdatedAt        string                   `json:"updatedAt"`
}

type preparedAWSCacheVolume struct {
	record awsCacheVolumeRecord
	fresh  bool
}

func newAWSCacheVolumeLifecycle() core.AWSCacheVolumeLifecycle {
	return &awsCacheVolumeLifecycle{}
}

func (l *awsCacheVolumeLifecycle) Prepare(ctx context.Context, cloud core.AWSCacheVolumeCloud, req core.AWSCacheVolumePrepareRequest) (core.AWSCacheVolumePlan, error) {
	if strings.TrimSpace(req.LeaseID) == "" || strings.TrimSpace(req.RepoScope) == "" {
		return core.AWSCacheVolumePlan{}, core.Exit(2, "AWS cache volumes require an exact lease and opaque repository scope")
	}
	if strings.TrimSpace(req.SSHUser) == "" {
		return core.AWSCacheVolumePlan{}, core.Exit(2, "AWS cache volumes require an exact SSH user")
	}
	if len(req.Volumes) > awsCacheVolumeMaxBindings {
		return core.AWSCacheVolumePlan{}, core.Exit(2, "AWS cache volumes support at most %d bindings per lease", awsCacheVolumeMaxBindings)
	}
	seenNames := map[string]bool{}
	seenPaths := map[string]bool{}
	for _, volume := range req.Volumes {
		if err := validateAWSCacheVolumeConfig(volume); err != nil {
			return core.AWSCacheVolumePlan{}, err
		}
		if err := validateAWSCacheVolumeMountPath(volume, req.SSHUser, req.WorkRoot); err != nil {
			return core.AWSCacheVolumePlan{}, err
		}
		name := strings.TrimSpace(volume.Name)
		if name == "" {
			name = strings.TrimSpace(volume.Key)
		}
		path := filepath.Clean(strings.TrimSpace(volume.Path))
		if seenNames[name] || seenPaths[path] {
			return core.AWSCacheVolumePlan{}, core.Exit(2, "AWS cache volumes require unique names and mount paths")
		}
		seenNames[name] = true
		seenPaths[path] = true
	}
	if err := cloud.ValidateCacheVolumeInstanceType(ctx, req.ServerType); err != nil {
		return core.AWSCacheVolumePlan{}, err
	}
	if strings.TrimSpace(req.Region) == "" || strings.TrimSpace(req.AvailabilityZone) == "" {
		return core.AWSCacheVolumePlan{}, core.Exit(2, "AWS cache volumes require an exact region and availability zone")
	}
	accountID, err := cloud.CallerAccountID(ctx)
	if err != nil {
		return core.AWSCacheVolumePlan{}, fmt.Errorf("bind AWS cache volume tenant: %w", err)
	}
	unlock, err := shared.LockOperation(ctx, "aws", "cache-volumes.lock", "AWS cache volume registry")
	if err != nil {
		return core.AWSCacheVolumePlan{}, err
	}
	defer unlock()
	registry, path, err := loadAWSCacheVolumeRegistry()
	if err != nil {
		return core.AWSCacheVolumePlan{}, err
	}
	prepared := make([]preparedAWSCacheVolume, 0, len(req.Volumes))
	for _, volume := range req.Volumes {
		identity := awsCacheVolumeIdentity(registry.InstallationID, accountID, req.RepoScope, volume.Key)
		candidate, fresh, err := reserveAWSCacheVolume(ctx, cloud, registry, path, req, volume, identity)
		if err != nil {
			rollbackPreparedAWSCacheVolumes(ctx, cloud, registry, path, prepared)
			return core.AWSCacheVolumePlan{}, err
		}
		prepared = append(prepared, preparedAWSCacheVolume{record: *candidate, fresh: fresh})
	}
	return awsCacheVolumePlan(prepared, req.LeaseID, req.Region, req.AvailabilityZone, req.SSHUser), nil
}

func rollbackPreparedAWSCacheVolumes(ctx context.Context, cloud core.AWSCacheVolumeCloud, registry *awsCacheVolumeRegistry, path string, prepared []preparedAWSCacheVolume) {
	for _, item := range prepared {
		record := findAWSCacheVolumeRecordByMember(registry, item.record.MemberID)
		if record == nil {
			continue
		}
		if record.VolumeID == "" {
			record.State = core.AWSCacheVolumeQuarantined
			continue
		}
		volume, err := cloud.DescribeCacheVolume(ctx, record.VolumeID)
		if err != nil {
			markAWSCacheVolumeRetryable(record, err)
			continue
		}
		if len(volume.Attachments) != 0 || !awsCacheVolumeTagsMatch(record, volume.Tags) {
			record.State = core.AWSCacheVolumeQuarantined
			continue
		}
		record.State = core.AWSCacheVolumeAvailable
		record.LeaseID = ""
		record.InstanceID = ""
		record.PurgeOnRelease = false
		record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_ = writeAWSCacheVolumeRegistry(path, registry)
}

func (l *awsCacheVolumeLifecycle) Attach(ctx context.Context, cloud core.AWSCacheVolumeCloud, plan core.AWSCacheVolumePlan, instanceID string) error {
	unlock, err := shared.LockOperation(ctx, "aws", "cache-volumes.lock", "AWS cache volume registry")
	if err != nil {
		return err
	}
	defer unlock()
	registry, path, err := loadAWSCacheVolumeRegistry()
	if err != nil {
		return err
	}
	for index, binding := range plan.Bindings {
		record := findAWSCacheVolumeRecord(registry, binding.VolumeID)
		if record == nil || record.LeaseID != plan.LeaseID || record.Generation != binding.Generation || record.AvailabilityZone != plan.AvailabilityZone || record.State != core.AWSCacheVolumeReserving {
			return core.Exit(4, "AWS cache volume %s no longer matches its durable reservation", binding.VolumeID)
		}
		live, err := cloud.DescribeCacheVolume(ctx, binding.VolumeID)
		if err != nil || !awsCacheVolumeReusable(record, live) {
			if err != nil {
				markAWSCacheVolumeRetryable(record, err)
			} else {
				record.State = core.AWSCacheVolumeQuarantined
				record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			}
			_ = writeAWSCacheVolumeRegistry(path, registry)
			if err != nil {
				return err
			}
			return core.Exit(4, "AWS cache volume %s live properties no longer match its durable reservation", binding.VolumeID)
		}
		record.State = core.AWSCacheVolumeAttached
		record.InstanceID = instanceID
		record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
			return err
		}
		device := fmt.Sprintf("/dev/sd%c", 'f'+rune(index))
		if err := attachAWSCacheVolume(ctx, cloud, binding.VolumeID, instanceID, device); err != nil {
			record.State = core.AWSCacheVolumeQuarantined
			record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = writeAWSCacheVolumeRegistry(path, registry)
			return err
		}
		if err := waitAWSCacheVolume(ctx, cloud, binding.VolumeID, func(volume core.AWSCacheVolume) bool {
			return awsCacheVolumePropertiesMatch(record, volume) &&
				len(volume.Attachments) == 1 &&
				volume.Attachments[0] == instanceID
		}); err != nil {
			record.State = core.AWSCacheVolumeQuarantined
			record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = writeAWSCacheVolumeRegistry(path, registry)
			return err
		}
	}
	return nil
}

func (l *awsCacheVolumeLifecycle) Release(ctx context.Context, cloud core.AWSCacheVolumeCloud, leaseID string, purge bool) error {
	if strings.TrimSpace(leaseID) == "" {
		return core.Exit(2, "AWS cache volume release requires an exact lease ID")
	}
	unlock, err := shared.LockOperation(ctx, "aws", "cache-volumes.lock", "AWS cache volume registry")
	if err != nil {
		return err
	}
	defer unlock()
	registry, path, err := loadAWSCacheVolumeRegistry()
	if err != nil {
		return err
	}
	var releaseErrs []error
	for index := range registry.Records {
		record := &registry.Records[index]
		if record.LeaseID != leaseID {
			continue
		}
		if record.VolumeID == "" {
			matches, findErr := cloud.FindCacheVolumes(ctx, record.AvailabilityZone, awsCacheVolumeTags(record))
			if findErr != nil || len(matches) != 1 {
				if findErr != nil {
					markAWSCacheVolumeRetryable(record, findErr)
				} else if len(matches) > 1 {
					record.State = core.AWSCacheVolumeQuarantined
				} else if record.State != core.AWSCacheVolumeQuarantined {
					record.State = core.AWSCacheVolumeReserving
				}
				if findErr != nil {
					releaseErrs = append(releaseErrs, findErr)
				} else if len(matches) > 1 {
					releaseErrs = append(releaseErrs, fmt.Errorf("AWS cache volume member %s resolved to %d volumes", record.MemberID, len(matches)))
				}
				continue
			}
			if !awsCacheVolumePropertiesMatch(record, matches[0]) {
				record.State = core.AWSCacheVolumeQuarantined
				record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
				releaseErrs = append(releaseErrs, fmt.Errorf("AWS cache volume member %s resolved to an incompatible volume", record.MemberID))
				continue
			}
			record.VolumeID = matches[0].ID
			if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
				return err
			}
		}
		volume, describeErr := cloud.DescribeCacheVolume(ctx, record.VolumeID)
		if describeErr != nil {
			if record.State == core.AWSCacheVolumeDeleting && awsCacheVolumeNotFound(describeErr) {
				record.VolumeID = ""
				record.LeaseID = ""
				record.InstanceID = ""
				continue
			}
			markAWSCacheVolumeRetryable(record, describeErr)
			releaseErrs = append(releaseErrs, describeErr)
			continue
		}
		if !awsCacheVolumePropertiesMatch(record, volume) {
			record.State = core.AWSCacheVolumeQuarantined
			releaseErrs = append(releaseErrs, fmt.Errorf("AWS cache volume %s ownership evidence changed", record.VolumeID))
			continue
		}
		if len(volume.Attachments) > 1 || (len(volume.Attachments) == 1 && volume.Attachments[0] != record.InstanceID) {
			record.State = core.AWSCacheVolumeQuarantined
			releaseErrs = append(releaseErrs, fmt.Errorf("AWS cache volume %s has an external attachment", record.VolumeID))
			continue
		}
		if len(volume.Attachments) == 1 {
			if err := cloud.DetachCacheVolume(ctx, record.VolumeID, record.InstanceID); err != nil {
				record.State = core.AWSCacheVolumeQuarantined
				releaseErrs = append(releaseErrs, err)
				continue
			}
			if err := waitAWSCacheVolume(ctx, cloud, record.VolumeID, func(current core.AWSCacheVolume) bool {
				return awsCacheVolumePropertiesMatch(record, current) && len(current.Attachments) == 0
			}); err != nil {
				record.State = core.AWSCacheVolumeQuarantined
				releaseErrs = append(releaseErrs, err)
				continue
			}
		}
		if purge || record.PurgeOnRelease {
			record.State = core.AWSCacheVolumeDeleting
			record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
				return err
			}
			if err := cloud.DeleteCacheVolume(ctx, record.VolumeID); err != nil {
				if awsCacheVolumeNotFound(err) {
					record.VolumeID = ""
					record.LeaseID = ""
					record.InstanceID = ""
					continue
				}
				releaseErrs = append(releaseErrs, err)
				continue
			}
			record.VolumeID = ""
			record.LeaseID = ""
			record.InstanceID = ""
			continue
		}
		record.State = core.AWSCacheVolumeAvailable
		record.LeaseID = ""
		record.InstanceID = ""
		record.PurgeOnRelease = false
		record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	registry.Records = slicesDeleteEmptyAWSCacheVolumes(registry.Records)
	if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
		return err
	}
	return errorsJoin(releaseErrs)
}

func (l *awsCacheVolumeLifecycle) GarbageCollect(ctx context.Context, cloud core.AWSCacheVolumeCloud, region string, cutoff time.Time, dryRun bool) ([]string, error) {
	accountID, err := cloud.CallerAccountID(ctx)
	if err != nil {
		return nil, err
	}
	unlock, err := shared.LockOperation(ctx, "aws", "cache-volumes.lock", "AWS cache volume registry")
	if err != nil {
		return nil, err
	}
	defer unlock()
	registry, path, err := loadAWSCacheVolumeRegistry()
	if err != nil {
		return nil, err
	}
	tenantDigest := digestAWSCacheValue(registry.InstallationID, "tenant", accountID)
	var deleted []string
	var gcErrs []error
	for index := range registry.Records {
		record := &registry.Records[index]
		updatedAt, parseErr := time.Parse(time.RFC3339Nano, record.UpdatedAt)
		deletionTombstone := record.InstanceID == "" && record.State == core.AWSCacheVolumeDeleting
		reusableCandidate := record.InstanceID == "" &&
			(record.State == core.AWSCacheVolumeAvailable || record.State == core.AWSCacheVolumeQuarantined)
		staleReservation := record.InstanceID == "" && record.State == core.AWSCacheVolumeReserving
		if parseErr != nil ||
			record.Region != region ||
			record.TenantDigest != tenantDigest ||
			(!reusableCandidate && !staleReservation && !deletionTombstone) ||
			(!deletionTombstone && !updatedAt.Before(cutoff)) {
			continue
		}
		if record.VolumeID == "" {
			matches, findErr := cloud.FindCacheVolumes(ctx, record.AvailabilityZone, awsCacheVolumeTags(record))
			if findErr != nil {
				if !dryRun {
					markAWSCacheVolumeRetryable(record, findErr)
				}
				gcErrs = append(gcErrs, findErr)
				continue
			}
			if len(matches) != 1 {
				if len(matches) > 1 && !dryRun {
					record.State = core.AWSCacheVolumeQuarantined
					record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
				} else if len(matches) == 0 && !dryRun {
					record.State = core.AWSCacheVolumeDeleting
					record.VolumeID = ""
				}
				continue
			}
			record.VolumeID = matches[0].ID
		}
		volume, describeErr := cloud.DescribeCacheVolume(ctx, record.VolumeID)
		if describeErr != nil {
			if deletionTombstone && awsCacheVolumeNotFound(describeErr) && !dryRun {
				record.VolumeID = ""
				continue
			}
			if !dryRun {
				markAWSCacheVolumeRetryable(record, describeErr)
			}
			gcErrs = append(gcErrs, describeErr)
			continue
		}
		if len(volume.Attachments) != 0 ||
			!awsCacheVolumePropertiesMatch(record, volume) {
			if !dryRun {
				record.State = core.AWSCacheVolumeQuarantined
				record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			}
			continue
		}
		deleted = append(deleted, record.VolumeID)
		if dryRun {
			continue
		}
		record.State = core.AWSCacheVolumeDeleting
		record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
			return deleted, err
		}
		if err := cloud.DeleteCacheVolume(ctx, record.VolumeID); err != nil {
			gcErrs = append(gcErrs, err)
			continue
		}
		record.VolumeID = ""
	}
	if !dryRun {
		registry.Records = slicesDeleteEmptyAWSCacheVolumes(registry.Records)
		if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
			return deleted, err
		}
	}
	return deleted, errors.Join(gcErrs...)
}

func reserveAWSCacheVolume(ctx context.Context, cloud core.AWSCacheVolumeCloud, registry *awsCacheVolumeRegistry, path string, req core.AWSCacheVolumePrepareRequest, volume core.CacheVolumeConfig, identity [4]string) (*awsCacheVolumeRecord, bool, error) {
	var maxGeneration int64
	memberCount := 0
	var pending *awsCacheVolumeRecord
	for index := range registry.Records {
		record := &registry.Records[index]
		if !awsCacheVolumeBaseIdentityMatches(record, identity) || record.Region != req.Region || record.AvailabilityZone != req.AvailabilityZone {
			continue
		}
		maxGeneration = max(maxGeneration, record.Generation)
		if record.ABIDigest != identity[3] {
			record.State = core.AWSCacheVolumeQuarantined
			record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
				return nil, false, err
			}
			continue
		}
		requestedSizeGB := max(volume.SizeGB, 1)
		if record.SizeGB < requestedSizeGB {
			record.State = core.AWSCacheVolumeQuarantined
			record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
				return nil, false, err
			}
			continue
		}
		if record.State != core.AWSCacheVolumeQuarantined &&
			(record.VolumeID != "" || record.State == core.AWSCacheVolumeReserving) {
			memberCount++
		}
		if record.State == core.AWSCacheVolumeReserving && record.LeaseID == req.LeaseID {
			record.Name = volume.Name
			record.Path = volume.Path
			record.PurgeOnRelease = req.PurgeOnRelease
			if record.VolumeID != "" {
				live, err := cloud.DescribeCacheVolume(ctx, record.VolumeID)
				if err != nil {
					markAWSCacheVolumeRetryable(record, err)
					if writeErr := writeAWSCacheVolumeRegistry(path, registry); writeErr != nil {
						return nil, false, errors.Join(err, writeErr)
					}
					return nil, false, err
				}
				if awsCacheVolumeReusable(record, live) {
					return record, false, nil
				}
				record.State = core.AWSCacheVolumeQuarantined
				record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
				if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
					return nil, false, err
				}
				continue
			}
			pending = record
			continue
		}
		if record.State != core.AWSCacheVolumeAvailable || record.VolumeID == "" {
			continue
		}
		live, err := cloud.DescribeCacheVolume(ctx, record.VolumeID)
		if err != nil {
			markAWSCacheVolumeRetryable(record, err)
			if writeErr := writeAWSCacheVolumeRegistry(path, registry); writeErr != nil {
				return nil, false, errors.Join(err, writeErr)
			}
			return nil, false, err
		}
		if !awsCacheVolumeReusable(record, live) {
			record.State = core.AWSCacheVolumeQuarantined
			record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
				return nil, false, err
			}
			continue
		}
		record.State = core.AWSCacheVolumeReserving
		record.LeaseID = req.LeaseID
		record.Name = volume.Name
		record.Path = volume.Path
		record.PurgeOnRelease = req.PurgeOnRelease
		record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
			return nil, false, err
		}
		return record, false, nil
	}
	if pending == nil && memberCount >= awsCacheVolumeMaxMembers {
		return nil, false, core.Exit(5, "AWS cache volume %q has no available members in %s", volume.Name, req.AvailabilityZone)
	}
	stored := pending
	if stored == nil {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		record := awsCacheVolumeRecord{
			CacheSetID:       randomAWSCacheID("set"),
			MemberID:         randomAWSCacheID("member"),
			TenantDigest:     identity[0],
			RepoScopeDigest:  identity[1],
			KeyDigest:        identity[2],
			ABIDigest:        identity[3],
			Name:             volume.Name,
			Path:             volume.Path,
			SizeGB:           max(volume.SizeGB, 1),
			Region:           req.Region,
			AvailabilityZone: req.AvailabilityZone,
			Generation:       maxGeneration + 1,
			State:            core.AWSCacheVolumeReserving,
			LeaseID:          req.LeaseID,
			PurgeOnRelease:   req.PurgeOnRelease,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		registry.Records = append(registry.Records, record)
		stored = &registry.Records[len(registry.Records)-1]
		if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
			return nil, false, err
		}
	}
	matches, err := cloud.FindCacheVolumes(ctx, req.AvailabilityZone, awsCacheVolumeTags(stored))
	if err != nil {
		markAWSCacheVolumeRetryable(stored, err)
		if writeErr := writeAWSCacheVolumeRegistry(path, registry); writeErr != nil {
			return nil, false, errors.Join(err, writeErr)
		}
		return nil, false, err
	}
	if len(matches) > 1 {
		stored.State = core.AWSCacheVolumeQuarantined
		_ = writeAWSCacheVolumeRegistry(path, registry)
		return nil, false, core.Exit(4, "AWS cache volume member %s resolved to multiple volumes", stored.MemberID)
	}
	volumeID := ""
	if len(matches) == 1 {
		if !awsCacheVolumeReusable(stored, matches[0]) {
			stored.State = core.AWSCacheVolumeQuarantined
			stored.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = writeAWSCacheVolumeRegistry(path, registry)
			return nil, false, core.Exit(4, "AWS cache volume member %s resolved to an incompatible volume", stored.MemberID)
		}
		volumeID = matches[0].ID
	} else {
		volumeID, err = cloud.CreateCacheVolume(ctx, req.AvailabilityZone, int32(stored.SizeGB), awsCacheVolumeTags(stored), stored.MemberID)
	}
	if err != nil {
		// A timeout may still have created the exact tagged volume. Quarantine
		// the durable record for cleanup, but do not let it consume member capacity.
		markAWSCacheVolumeRetryable(stored, err)
		if writeErr := writeAWSCacheVolumeRegistry(path, registry); writeErr != nil {
			return nil, false, errors.Join(err, fmt.Errorf("persist failed AWS cache volume reservation: %w", writeErr))
		}
		return nil, false, err
	}
	stored.VolumeID = volumeID
	stored.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
		return nil, false, err
	}
	if err := waitAWSCacheVolume(ctx, cloud, volumeID, func(current core.AWSCacheVolume) bool {
		return awsCacheVolumeReusable(stored, current)
	}); err != nil {
		markAWSCacheVolumeRetryable(stored, err)
		_ = writeAWSCacheVolumeRegistry(path, registry)
		return nil, false, err
	}
	return stored, true, nil
}

func awsCacheVolumePlan(prepared []preparedAWSCacheVolume, leaseID, region, availabilityZone, sshUser string) core.AWSCacheVolumePlan {
	bindings := make([]core.AWSCacheVolumeBinding, 0, len(prepared))
	for _, item := range prepared {
		bindings = append(bindings, core.AWSCacheVolumeBinding{
			Name:       item.record.Name,
			Path:       item.record.Path,
			VolumeID:   item.record.VolumeID,
			Generation: item.record.Generation,
			ABI:        awsCacheVolumeABI,
		})
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Name < bindings[j].Name })
	return core.AWSCacheVolumePlan{
		ProtocolVersion:  core.AWSCacheVolumeProtocolVersion,
		LeaseID:          leaseID,
		Region:           region,
		AvailabilityZone: availabilityZone,
		Bindings:         bindings,
		Bootstrap:        awsCacheVolumeBootstrap(prepared, sshUser),
		ReadyChecks:      awsCacheVolumeReadyChecks(bindings),
	}
}

func awsCacheVolumeBootstrap(prepared []preparedAWSCacheVolume, sshUser string) string {
	var lines []string
	lines = append(lines, "    install -d -m 0755 /var/lib/crabbox/cache-volumes")
	for _, item := range prepared {
		record := item.record
		expectedSerial := strings.ReplaceAll(record.VolumeID, "-", "")
		lines = append(lines,
			"    cache_device=''",
			"    for cache_wait in $(seq 1 120); do",
			"      for cache_sys in /sys/block/nvme*n1; do",
			"        [ -r \"$cache_sys/device/serial\" ] || continue",
			fmt.Sprintf("        [ \"$(tr -d '[:space:]' < \"$cache_sys/device/serial\")\" = %s ] || continue", shellQuote(expectedSerial)),
			"        cache_device=\"/dev/${cache_sys##*/}\"",
			"        break",
			"      done",
			"      [ -n \"$cache_device\" ] && break",
			"      sleep 1",
			"    done",
			fmt.Sprintf("    [ -n \"$cache_device\" ] || { echo %s >&2; exit 1; }", shellQuote("cache volume "+record.VolumeID+" not attached")),
			"    root_source=\"$(findmnt -n -o SOURCE /)\"",
			"    [ \"$cache_device\" != \"$root_source\" ] || { echo 'refusing cache root device' >&2; exit 1; }",
			fmt.Sprintf("    cache_path=%s", shellQuote(record.Path)),
			"    cache_walk=/",
			"    old_ifs=$IFS; IFS=/",
			"    for cache_part in ${cache_path#/}; do",
			"      [ -n \"$cache_part\" ] || continue",
			"      cache_walk=\"${cache_walk%/}/$cache_part\"",
			"      [ ! -L \"$cache_walk\" ] || { echo 'refusing symlink cache mount path' >&2; exit 1; }",
			"    done",
			"    IFS=$old_ifs",
			"    cache_fs=\"$(blkid -o value -s TYPE \"$cache_device\" 2>/dev/null || true)\"",
		)
		lines = append(lines,
			"    if [ -z \"$cache_fs\" ]; then",
			"      mkfs.ext4 -F \"$cache_device\"; cache_fs=ext4",
			"    elif [ \"$cache_fs\" = ext4 ]; then",
			"      if e2fsck -p \"$cache_device\"; then :; else cache_fsck=$?; [ \"$cache_fsck\" -le 1 ] || { wipefs -a \"$cache_device\"; mkfs.ext4 -F \"$cache_device\"; }; fi",
			"    fi",
		)
		lines = append(lines,
			"    [ \"$cache_fs\" = ext4 ] || { echo 'cache volume filesystem is not ext4' >&2; exit 1; }",
			"    install -d -m 0755 \"$cache_path\"",
			"    [ ! -L \"$cache_path\" ] || { echo 'refusing symlink cache mount path' >&2; exit 1; }",
			"    [ \"$(readlink -f -- \"$cache_path\")\" = \"$cache_path\" ] || { echo 'refusing indirect cache mount path' >&2; exit 1; }",
			"    mount -o nodev,nosuid \"$cache_device\" \"$cache_path\"",
		)
		if !item.fresh {
			lines = append(lines,
				fmt.Sprintf("    if [ ! -f \"$cache_path/.crabbox-cache-abi\" ] || [ \"$(cat \"$cache_path/.crabbox-cache-abi\")\" != %s ]; then", shellQuote(awsCacheVolumeABI)),
				"      umount \"$cache_path\"",
				"      wipefs -a \"$cache_device\"",
				"      mkfs.ext4 -F \"$cache_device\"",
				"      mount -o nodev,nosuid \"$cache_device\" \"$cache_path\"",
				"    fi",
			)
		}
		lines = append(lines,
			fmt.Sprintf("    printf '%%s\\n' %s > \"$cache_path/.crabbox-cache-abi\"", shellQuote(awsCacheVolumeABI)),
			fmt.Sprintf("    chown %s \"$cache_path\"", shellQuote(sshUser+":"+sshUser)),
		)
	}
	return strings.Join(lines, "\n")
}

func attachAWSCacheVolume(ctx context.Context, cloud core.AWSCacheVolumeCloud, volumeID, instanceID, device string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := cloud.AttachCacheVolume(ctx, volumeID, instanceID, device)
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "IncorrectInstanceState") || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func awsCacheVolumeReadyChecks(bindings []core.AWSCacheVolumeBinding) string {
	lines := make([]string, 0, len(bindings)*2)
	for _, binding := range bindings {
		lines = append(lines,
			fmt.Sprintf("      mountpoint -q %s", shellQuote(binding.Path)),
			fmt.Sprintf("      test \"$(cat %s/.crabbox-cache-abi)\" = %s", shellQuote(binding.Path), shellQuote(awsCacheVolumeABI)),
		)
	}
	return strings.Join(lines, "\n")
}

func loadAWSCacheVolumeRegistry() (*awsCacheVolumeRegistry, string, error) {
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		return nil, "", err
	}
	root := filepath.Join(stateDir, "aws")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, "", core.Exit(2, "create AWS cache volume state directory: %v", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, "", err
	}
	path := filepath.Join(root, awsCacheVolumeStateFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &awsCacheVolumeRegistry{SchemaVersion: 1, InstallationID: randomAWSCacheID("install")}, path, nil
	}
	if err != nil {
		return nil, "", err
	}
	var registry awsCacheVolumeRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, "", core.Exit(2, "decode AWS cache volume registry: %v", err)
	}
	if registry.SchemaVersion != 1 || registry.InstallationID == "" {
		return nil, "", core.Exit(2, "unsupported AWS cache volume registry schema")
	}
	return &registry, path, nil
}

func writeAWSCacheVolumeRegistry(path string, registry *awsCacheVolumeRegistry) error {
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cache-volumes-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func validateAWSCacheVolumeConfig(volume core.CacheVolumeConfig) error {
	name := strings.TrimSpace(volume.Name)
	if name == "" {
		name = strings.TrimSpace(volume.Key)
	}
	mountPath := path.Clean(strings.TrimSpace(volume.Path))
	if name == "" || strings.TrimSpace(volume.Key) == "" || !path.IsAbs(mountPath) || mountPath != strings.TrimSpace(volume.Path) {
		return core.Exit(2, "AWS cache volume requires a stable name, key, and clean absolute path")
	}
	for _, unsafe := range []string{"/", "/boot", "/dev", "/etc", "/proc", "/run", "/sys", "/usr"} {
		if mountPath == unsafe || strings.HasPrefix(mountPath, unsafe+"/") {
			return core.Exit(2, "AWS cache volume %q uses unsafe mount path %s", name, mountPath)
		}
	}
	return nil
}

func validateAWSCacheVolumeMountPath(volume core.CacheVolumeConfig, sshUser, workRoot string) error {
	name := strings.TrimSpace(volume.Name)
	if name == "" {
		name = strings.TrimSpace(volume.Key)
	}
	mountPath := path.Clean(strings.TrimSpace(volume.Path))
	sshHome := "/home/" + strings.TrimSpace(sshUser)
	if strings.TrimSpace(sshUser) == "root" {
		sshHome = "/root"
	}
	protected := []string{"/root", sshHome, path.Clean(strings.TrimSpace(workRoot))}
	for _, root := range protected {
		if !path.IsAbs(root) {
			continue
		}
		if mountPath == root || strings.HasPrefix(root, mountPath+"/") {
			return core.Exit(2, "AWS cache volume %q mount path %s would hide runtime root %s", name, mountPath, root)
		}
	}
	sensitive := []string{path.Join(sshHome, ".ssh"), "/var/lib/crabbox", "/var/lib/cloud"}
	for _, root := range sensitive {
		if mountPath == root || strings.HasPrefix(mountPath, root+"/") || strings.HasPrefix(root, mountPath+"/") {
			return core.Exit(2, "AWS cache volume %q mount path %s overlaps sensitive runtime tree %s", name, mountPath, root)
		}
	}
	return nil
}

func awsCacheVolumeIdentity(installationID, accountID, repoScope, key string) [4]string {
	return [4]string{
		digestAWSCacheValue(installationID, "tenant", accountID),
		digestAWSCacheValue(installationID, "repo", repoScope),
		digestAWSCacheValue(installationID, "key", key),
		awsCacheVolumeABIDigest(),
	}
}

func awsCacheVolumeABIDigest() string {
	sum := sha256.Sum256([]byte(awsCacheVolumeABI))
	return hex.EncodeToString(sum[:16])
}

func digestAWSCacheValue(installationID, kind, value string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{installationID, kind, strings.TrimSpace(value)}, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func awsCacheVolumeBaseIdentityMatches(record *awsCacheVolumeRecord, identity [4]string) bool {
	return record.TenantDigest == identity[0] &&
		record.RepoScopeDigest == identity[1] &&
		record.KeyDigest == identity[2]
}

func awsCacheVolumeTags(record *awsCacheVolumeRecord) map[string]string {
	return map[string]string{
		"crabbox":                  "true",
		"created_by":               "crabbox",
		"crabbox_cache_set":        record.CacheSetID,
		"crabbox_cache_member":     record.MemberID,
		"crabbox_cache_generation": fmt.Sprintf("%d", record.Generation),
		"crabbox_cache_abi":        record.ABIDigest,
	}
}

func awsCacheVolumeTagsMatch(record *awsCacheVolumeRecord, tags map[string]string) bool {
	for key, value := range awsCacheVolumeTags(record) {
		if tags[key] != value {
			return false
		}
	}
	return true
}

func awsCacheVolumePropertiesMatch(record *awsCacheVolumeRecord, volume core.AWSCacheVolume) bool {
	return volume.AvailabilityZone == record.AvailabilityZone &&
		volume.Encrypted &&
		volume.VolumeType == "gp3" &&
		volume.SizeGB == int32(record.SizeGB) &&
		!volume.MultiAttach &&
		awsCacheVolumeTagsMatch(record, volume.Tags)
}

func awsCacheVolumeReusable(record *awsCacheVolumeRecord, volume core.AWSCacheVolume) bool {
	return awsCacheVolumePropertiesMatch(record, volume) &&
		volume.State == core.AWSCacheVolumeAvailable &&
		len(volume.Attachments) == 0
}

func markAWSCacheVolumeRetryable(record *awsCacheVolumeRecord, err error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record.State = core.AWSCacheVolumeQuarantined
	record.LastError = err.Error()
	record.LastErrorAt = now
	record.RetryCount++
	record.UpdatedAt = now
}

func waitAWSCacheVolume(ctx context.Context, cloud core.AWSCacheVolumeCloud, volumeID string, ready func(core.AWSCacheVolume) bool) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		volume, err := cloud.DescribeCacheVolume(ctx, volumeID)
		if err != nil {
			return err
		}
		if ready(volume) {
			return nil
		}
		if time.Now().After(deadline) {
			return core.Exit(5, "timed out waiting for AWS cache volume %s", volumeID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func findAWSCacheVolumeRecord(registry *awsCacheVolumeRegistry, volumeID string) *awsCacheVolumeRecord {
	for index := range registry.Records {
		if registry.Records[index].VolumeID == volumeID {
			return &registry.Records[index]
		}
	}
	return nil
}

func findAWSCacheVolumeRecordByMember(registry *awsCacheVolumeRegistry, memberID string) *awsCacheVolumeRecord {
	for index := range registry.Records {
		if registry.Records[index].MemberID == memberID {
			return &registry.Records[index]
		}
	}
	return nil
}

func slicesDeleteEmptyAWSCacheVolumes(records []awsCacheVolumeRecord) []awsCacheVolumeRecord {
	out := records[:0]
	for _, record := range records {
		if record.State == core.AWSCacheVolumeDeleting && record.VolumeID == "" {
			continue
		}
		out = append(out, record)
	}
	return out
}

func randomAWSCacheID(prefix string) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("read crypto randomness: %v", err))
	}
	return prefix + "-" + hex.EncodeToString(raw[:])
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func errorsJoin(errs []error) error {
	var nonNil []error
	for _, err := range errs {
		if err != nil {
			nonNil = append(nonNil, err)
		}
	}
	if len(nonNil) == 0 {
		return nil
	}
	return errors.Join(nonNil...)
}

func awsCacheVolumeNotFound(err error) bool {
	message := err.Error()
	return strings.Contains(message, "InvalidVolume.NotFound") || strings.Contains(message, "cache volume not found")
}
