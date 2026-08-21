package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const AWSCacheVolumeProtocolVersion = 1

type AWSCacheVolumeState string

const (
	AWSCacheVolumeReserving   AWSCacheVolumeState = "reserving"
	AWSCacheVolumeAvailable   AWSCacheVolumeState = "available"
	AWSCacheVolumeAttached    AWSCacheVolumeState = "attached"
	AWSCacheVolumeQuarantined AWSCacheVolumeState = "quarantined"
	AWSCacheVolumeDeleting    AWSCacheVolumeState = "deleting"
)

type AWSCacheVolumeBinding struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	VolumeID   string `json:"volumeID"`
	Generation int64  `json:"generation"`
	ABI        string `json:"abi"`
}

type AWSCacheVolumePlan struct {
	ProtocolVersion  int                     `json:"protocolVersion"`
	LeaseID          string                  `json:"leaseID"`
	Region           string                  `json:"region"`
	AvailabilityZone string                  `json:"availabilityZone"`
	Bindings         []AWSCacheVolumeBinding `json:"bindings"`
	Bootstrap        string                  `json:"-"`
	ReadyChecks      string                  `json:"-"`
}

type AWSCacheVolume struct {
	ID               string
	State            AWSCacheVolumeState
	AvailabilityZone string
	Encrypted        bool
	VolumeType       string
	SizeGB           int32
	MultiAttach      bool
	Attachments      []string
	Tags             map[string]string
}

type AWSCacheVolumeCloud interface {
	CallerAccountID(context.Context) (string, error)
	ValidateCacheVolumeInstanceType(context.Context, string) error
	CreateCacheVolume(context.Context, string, int32, map[string]string, string) (string, error)
	FindCacheVolumes(context.Context, string, map[string]string) ([]AWSCacheVolume, error)
	DescribeCacheVolume(context.Context, string) (AWSCacheVolume, error)
	AttachCacheVolume(context.Context, string, string, string) error
	DetachCacheVolume(context.Context, string, string) error
	DeleteCacheVolume(context.Context, string) error
}

type AWSCacheVolumePrepareRequest struct {
	LeaseID          string
	RepoScope        string
	Region           string
	AvailabilityZone string
	ServerType       string
	SSHUser          string
	WorkRoot         string
	Volumes          []CacheVolumeConfig
	PurgeOnRelease   bool
}

type AWSCacheVolumeLifecycle interface {
	Prepare(context.Context, AWSCacheVolumeCloud, AWSCacheVolumePrepareRequest) (AWSCacheVolumePlan, error)
	Attach(context.Context, AWSCacheVolumeCloud, AWSCacheVolumePlan, string) error
	Release(context.Context, AWSCacheVolumeCloud, string, bool) error
}

func OpaqueCacheRepoScope(repo Repo) string {
	material := strings.TrimSpace(repo.RemoteURL)
	if material == "" {
		material = strings.TrimSpace(repo.Root)
	}
	if material == "" {
		return ""
	}
	digest := sha256.Sum256([]byte("crabbox-cache-repo-scope-v1\x00" + material))
	return "repo-" + hex.EncodeToString(digest[:16])
}
