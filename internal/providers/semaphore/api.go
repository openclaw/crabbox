// Install: copy to internal/providers/semaphore/api.go
package semaphore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type apiClient struct {
	host  string
	token string
	http  *http.Client
	rt    core.Runtime
}

type jobInfo struct {
	ID    string
	Name  string
	State string
}

func newAPIClient(host, token string, rt core.Runtime) *apiClient {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	if rt.HTTP != nil {
		httpClient = rt.HTTP
	}
	return &apiClient{host: host, token: token, http: httpClient, rt: rt}
}

// CreateJob creates a standalone Semaphore job with a keepalive script.
// Returns the job ID.
func (c *apiClient) CreateJob(ctx context.Context, project, machine, osImage string, idleTimeout time.Duration) (string, error) {
	// Resolve project name to ID
	projectID, err := c.resolveProjectID(ctx, project)
	if err != nil {
		return "", fmt.Errorf("resolve project %q: %w", project, err)
	}

	durationSecs := int(idleTimeout.Seconds()) * 2 // max duration = 2x idle timeout
	idleSecs := int(idleTimeout.Seconds())

	keepalive := fmt.Sprintf(
		`echo crabbox-testbox-ready && touch /tmp/.testbox-activity && python3 -c "
import os, time, sys
max_duration = %d
idle_timeout = %d
start = time.time()
f = '/tmp/.testbox-activity'
while True:
    if time.time() - start >= max_duration:
        sys.exit(0)
    try:
        if time.time() - os.path.getmtime(f) >= idle_timeout:
            sys.exit(0)
    except: pass
    time.sleep(5)
"`, durationSecs, idleSecs)

	body := map[string]any{
		"apiVersion": "v1alpha",
		"kind":       "Job",
		"metadata":   map[string]string{"name": "crabbox testbox"},
		"spec": map[string]any{
			"project_id": projectID,
			"agent": map[string]any{
				"machine": map[string]string{
					"type":     machine,
					"os_image": osImage,
				},
			},
			"commands": []string{keepalive},
		},
	}

	var result struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
	}
	if err := c.post(ctx, "/api/v1alpha/jobs", body, &result); err != nil {
		return "", err
	}
	if result.Metadata.ID == "" {
		return "", fmt.Errorf("job creation returned no ID")
	}
	return result.Metadata.ID, nil
}

// WaitForRunning polls until the job reaches RUNNING state.
// Returns the SSH IP and port.
func (c *apiClient) WaitForRunning(ctx context.Context, jobID string, tick func()) (string, int, error) {
	for i := 0; i < 120; i++ {
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		tick()

		state, ip, port, err := c.GetJobStatus(ctx, jobID)
		if err != nil {
			continue
		}
		if state == "FINISHED" {
			return "", 0, core.Exit(5, "job %s finished before reaching RUNNING state", jobID)
		}
		if state == "RUNNING" {
			return ip, port, nil
		}
	}
	return "", 0, core.Exit(5, "job %s did not reach RUNNING state within timeout", jobID)
}

// GetJobStatus returns the job state, IP, and SSH port.
func (c *apiClient) GetJobStatus(ctx context.Context, jobID string) (state, ip string, sshPort int, err error) {
	var result struct {
		Status struct {
			State string `json:"state"`
			Agent struct {
				IP    string `json:"ip"`
				Ports []struct {
					Name   string `json:"name"`
					Number int    `json:"number"`
				} `json:"ports"`
			} `json:"agent"`
		} `json:"status"`
	}
	if err := c.get(ctx, "/api/v1alpha/jobs/"+jobID, &result); err != nil {
		return "", "", 0, err
	}
	port := 0
	for _, p := range result.Status.Agent.Ports {
		if p.Name == "ssh" {
			port = p.Number
		}
	}
	return result.Status.State, result.Status.Agent.IP, port, nil
}

// GetSSHKey returns the SSH private key for a job.
func (c *apiClient) GetSSHKey(ctx context.Context, jobID string) (string, error) {
	var result struct {
		Key string `json:"key"`
	}
	if err := c.get(ctx, "/api/v1alpha/jobs/"+jobID+"/debug_ssh_key", &result); err != nil {
		return "", err
	}
	if result.Key == "" {
		return "", fmt.Errorf("no SSH key returned for job %s", jobID)
	}
	return result.Key, nil
}

// StopJob stops a running job.
func (c *apiClient) StopJob(ctx context.Context, jobID string) error {
	return c.post(ctx, "/api/v1alpha/jobs/"+jobID+"/stop", nil, nil)
}

// ListRunningJobs returns currently running jobs.
func (c *apiClient) ListRunningJobs(ctx context.Context) ([]jobInfo, error) {
	var result struct {
		Jobs []struct {
			Metadata struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"jobs"`
	}
	if err := c.get(ctx, "/api/v1alpha/jobs?states=RUNNING", &result); err != nil {
		return nil, err
	}
	var jobs []jobInfo
	for _, j := range result.Jobs {
		jobs = append(jobs, jobInfo{
			ID:    j.Metadata.ID,
			Name:  j.Metadata.Name,
			State: j.Status.State,
		})
	}
	return jobs, nil
}

func (c *apiClient) resolveProjectID(ctx context.Context, name string) (string, error) {
	// Try direct GET by name
	var project struct {
		Metadata struct {
			ID string `json:"id"`
		} `json:"metadata"`
	}
	err := c.get(ctx, "/api/v1alpha/projects/"+name, &project)
	if err == nil && project.Metadata.ID != "" {
		return project.Metadata.ID, nil
	}

	// Fallback: list all and match by name
	var projects []struct {
		Metadata struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"metadata"`
	}
	if err := c.get(ctx, "/api/v1alpha/projects", &projects); err != nil {
		return "", err
	}
	for _, p := range projects {
		if p.Metadata.Name == name {
			return p.Metadata.ID, nil
		}
	}
	return "", fmt.Errorf("project %q not found", name)
}

func (c *apiClient) get(ctx context.Context, path string, target any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://"+c.host+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("User-Agent", "crabbox-semaphore-provider")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return fmt.Errorf("semaphore API %s returned %d: %s", path, resp.StatusCode, string(body))
	}
	if target != nil {
		return json.Unmarshal(body, target)
	}
	return nil
}

func (c *apiClient) post(ctx context.Context, path string, payload any, target any) error {
	var bodyReader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+c.host+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "crabbox-semaphore-provider")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return fmt.Errorf("semaphore API %s returned %d: %s", path, resp.StatusCode, string(body))
	}
	if target != nil {
		return json.Unmarshal(body, target)
	}
	return nil
}
