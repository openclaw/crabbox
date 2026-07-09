package fal

import (
	"bytes"
	"encoding/json"
	"fmt"
)

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

func (r *ListInstancesResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		NextCursor json.RawMessage `json:"next_cursor"`
		HasMore    json.RawMessage `json:"has_more"`
		Instances  json.RawMessage `json:"instances"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	for _, field := range []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "next_cursor", raw: wire.NextCursor},
		{name: "has_more", raw: wire.HasMore},
		{name: "instances", raw: wire.Instances},
	} {
		if len(field.raw) == 0 {
			return fmt.Errorf("fal list response missing required field %q", field.name)
		}
	}

	var nextCursor *string
	if !bytes.Equal(bytes.TrimSpace(wire.NextCursor), []byte("null")) {
		var value string
		if err := json.Unmarshal(wire.NextCursor, &value); err != nil {
			return fmt.Errorf("fal list response field %q: %w", "next_cursor", err)
		}
		nextCursor = &value
	}
	var hasMore bool
	if bytes.Equal(bytes.TrimSpace(wire.HasMore), []byte("null")) {
		return fmt.Errorf("fal list response field %q must not be null", "has_more")
	}
	if err := json.Unmarshal(wire.HasMore, &hasMore); err != nil {
		return fmt.Errorf("fal list response field %q: %w", "has_more", err)
	}
	if bytes.Equal(bytes.TrimSpace(wire.Instances), []byte("null")) {
		return fmt.Errorf("fal list response field %q must not be null", "instances")
	}
	var instances []ComputeInstance
	if err := json.Unmarshal(wire.Instances, &instances); err != nil {
		return fmt.Errorf("fal list response field %q: %w", "instances", err)
	}

	*r = ListInstancesResponse{NextCursor: nextCursor, HasMore: hasMore, Instances: instances}
	return nil
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
