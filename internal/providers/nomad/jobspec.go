package nomad

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
)

const (
	leasePrefix = "cbx_"
	jobIDPrefix = "crabbox-"

	metadataManaged   = "crabbox.managed"
	metadataLeaseID   = "crabbox.lease_id"
	metadataSlug      = "crabbox.slug"
	metadataProvider  = "crabbox.provider"
	metadataScope     = "crabbox.scope"
	metadataNamespace = "crabbox.namespace"
	metadataRegion    = "crabbox.region"
	metadataJobID     = "crabbox.job_id"
	metadataTask      = "crabbox.task"
	metadataWorkdir   = "crabbox.workdir"
	metadataExpiresAt = "crabbox.expires_at"
)

type jobSpecInput struct {
	LeaseID   string
	Slug      string
	JobID     string
	ExpiresAt time.Time
}

func buildJobSpec(cfg Config, in jobSpecInput) (*nomadapi.Job, error) {
	meta := ownershipMetadata(cfg, in)
	if template := strings.TrimSpace(cfg.Nomad.JobSpecTemplate); template != "" {
		return buildTemplateJobSpec(cfg, in, meta, template)
	}
	return defaultJobSpec(cfg, in.JobID, meta), nil
}

func defaultJobSpec(cfg Config, jobID string, meta map[string]string) *nomadapi.Job {
	region := strings.TrimSpace(cfg.Nomad.Region)
	namespace := strings.TrimSpace(cfg.Nomad.Namespace)
	jobType := nomadapi.JobTypeService
	count := 1
	taskName := strings.TrimSpace(cfg.Nomad.Task)
	driver := strings.TrimSpace(cfg.Nomad.Driver)
	image := strings.TrimSpace(cfg.Nomad.Image)
	cpu := cfg.Nomad.CPU
	memory := cfg.Nomad.MemoryMB
	disk := cfg.Nomad.DiskMB
	taskConfig := map[string]interface{}{
		"command": "/bin/sh",
		"args":    []string{"-lc", "mkdir -p " + shellQuote(cfg.Nomad.Workdir) + " && sleep infinity"},
	}
	if image != "" {
		taskConfig["image"] = image
	}
	job := &nomadapi.Job{
		ID:          stringPtr(jobID),
		Name:        stringPtr(jobID),
		Type:        stringPtr(jobType),
		Region:      stringPtr(region),
		Namespace:   stringPtr(namespace),
		Datacenters: append([]string(nil), cfg.Nomad.Datacenters...),
		NodePool:    stringPtr(strings.TrimSpace(cfg.Nomad.NodePool)),
		Meta:        meta,
		TaskGroups: []*nomadapi.TaskGroup{{
			Name:          stringPtr("crabbox"),
			Count:         intPtr(count),
			EphemeralDisk: &nomadapi.EphemeralDisk{SizeMB: intPtr(disk)},
			Meta:          meta,
			Tasks: []*nomadapi.Task{{
				Name:      taskName,
				Driver:    driver,
				Config:    taskConfig,
				Resources: &nomadapi.Resources{CPU: intPtr(cpu), MemoryMB: intPtr(memory), DiskMB: intPtr(disk)},
				Meta:      meta,
			}},
		}},
	}
	return job
}

func buildTemplateJobSpec(cfg Config, in jobSpecInput, meta map[string]string, template string) (*nomadapi.Job, error) {
	if ext := strings.ToLower(filepath.Ext(template)); ext != ".json" {
		return nil, exit(2, "nomad jobspec template %s must be JSON for offline validation in this phase", template)
	}
	data, err := os.ReadFile(template)
	if err != nil {
		return nil, exit(2, "read nomad jobspec template: %v", err)
	}
	rendered := renderJobSpecTemplate(string(data), cfg, in, meta)
	var job nomadapi.Job
	if err := json.Unmarshal([]byte(rendered), &job); err != nil {
		return nil, exit(2, "parse nomad jobspec template JSON: %v", err)
	}
	if err := validateJobSpecOwnership(cfg, in, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

func renderJobSpecTemplate(template string, cfg Config, in jobSpecInput, meta map[string]string) string {
	values := map[string]string{
		"{{.LeaseID}}":   in.LeaseID,
		"{{.Slug}}":      in.Slug,
		"{{.JobID}}":     in.JobID,
		"{{.Task}}":      cfg.Nomad.Task,
		"{{.Workdir}}":   cfg.Nomad.Workdir,
		"{{.Namespace}}": normalizeNamespace(cfg.Nomad.Namespace),
		"{{.Region}}":    normalizeRegion(cfg.Nomad.Region),
		"{{.Scope}}":     meta[metadataScope],
		"{{.ExpiresAt}}": meta[metadataExpiresAt],
	}
	out := template
	for placeholder, value := range values {
		out = strings.ReplaceAll(out, placeholder, value)
	}
	return out
}

func validateJobSpecOwnership(cfg Config, in jobSpecInput, job *nomadapi.Job) error {
	if job == nil {
		return exit(2, "nomad jobspec template produced an empty job")
	}
	if strings.TrimSpace(stringValue(job.ID)) != in.JobID {
		return exit(2, "nomad jobspec template must set job id to %s", in.JobID)
	}
	if len(job.TaskGroups) == 0 {
		return exit(2, "nomad jobspec template must define a task group")
	}
	if !metadataMatches(job.Meta, ownershipMetadata(cfg, in)) {
		return exit(2, "nomad jobspec template must include required Crabbox ownership metadata")
	}
	if _, ok := findTask(job, cfg.Nomad.Task); !ok {
		return exit(2, "nomad jobspec template must define task %q", cfg.Nomad.Task)
	}
	return nil
}

func ownershipMetadata(cfg Config, in jobSpecInput) map[string]string {
	expiresAt := ""
	if !in.ExpiresAt.IsZero() {
		expiresAt = in.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return map[string]string{
		metadataManaged:   "true",
		metadataLeaseID:   in.LeaseID,
		metadataSlug:      in.Slug,
		metadataProvider:  providerName,
		metadataScope:     scopeFingerprint(claimScope(cfg)),
		metadataNamespace: normalizeNamespace(cfg.Nomad.Namespace),
		metadataRegion:    normalizeRegion(cfg.Nomad.Region),
		metadataJobID:     in.JobID,
		metadataTask:      cfg.Nomad.Task,
		metadataWorkdir:   cfg.Nomad.Workdir,
		metadataExpiresAt: expiresAt,
	}
}

func metadataMatches(got, want map[string]string) bool {
	for key, value := range want {
		if strings.TrimSpace(got[key]) != value {
			return false
		}
	}
	return true
}

func findTask(job *nomadapi.Job, taskName string) (*nomadapi.Task, bool) {
	for _, group := range job.TaskGroups {
		for _, task := range group.Tasks {
			if task.Name == taskName {
				return task, true
			}
		}
	}
	return nil, false
}

func jobIDForLease(leaseID string) string {
	return jobIDPrefix + strings.ReplaceAll(strings.TrimPrefix(leaseID, leasePrefix), "_", "-")
}

func normalizeNamespace(namespace string) string {
	if namespace = strings.TrimSpace(namespace); namespace != "" {
		return namespace
	}
	return nomadapi.DefaultNamespace
}

func normalizeRegion(region string) string {
	if region = strings.TrimSpace(region); region != "" {
		return region
	}
	return "default"
}

func scopeFingerprint(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(sum[:])
}

func stringPtr(value string) *string { return &value }
func intPtr(value int) *int          { return &value }

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
