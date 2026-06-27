package fal

type InstanceStatus string

const (
	InstanceStatusReady        InstanceStatus = "ready"
	InstanceStatusInit         InstanceStatus = "init"
	InstanceStatusPending      InstanceStatus = "pending"
	InstanceStatusProvisioning InstanceStatus = "provisioning"
	InstanceStatusStopped      InstanceStatus = "stopped"
	InstanceStatusUnknown      InstanceStatus = "unknown"
)

func (s InstanceStatus) Known() bool {
	switch s {
	case InstanceStatusReady, InstanceStatusInit, InstanceStatusPending, InstanceStatusProvisioning, InstanceStatusStopped, InstanceStatusUnknown:
		return true
	default:
		return false
	}
}

type InstanceType string

const (
	InstanceTypeH100x1 InstanceType = "gpu_1x_h100_sxm5"
	InstanceTypeH100x8 InstanceType = "gpu_8x_h100_sxm5"
)

type Sector string

const (
	Sector1 Sector = "sector_1"
	Sector2 Sector = "sector_2"
	Sector3 Sector = "sector_3"
)

type ComputeInstance struct {
	ID                  string         `json:"id"`
	InstanceType        InstanceType   `json:"instance_type"`
	Region              string         `json:"region"`
	Sector              Sector         `json:"sector,omitempty"`
	IP                  string         `json:"ip,omitempty"`
	Status              InstanceStatus `json:"status"`
	CreatorUserNickname string         `json:"creator_user_nickname,omitempty"`
}

type ListInstancesResponse struct {
	NextCursor *string           `json:"next_cursor"`
	HasMore    bool              `json:"has_more"`
	Instances  []ComputeInstance `json:"instances"`
}

type CreateInstanceRequest struct {
	InstanceType InstanceType `json:"instance_type"`
	SSHKey       string       `json:"ssh_key"`
	Sector       Sector       `json:"sector,omitempty"`
}

type APIErrorBody struct {
	Error APIErrorDetail `json:"error"`
}

type APIErrorDetail struct {
	Type      string `json:"type"`
	Message   string `json:"message"`
	DocsURL   string `json:"docs_url,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}
