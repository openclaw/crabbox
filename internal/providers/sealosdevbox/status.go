package sealosdevbox

import "strings"

type devboxList struct {
	Items []devboxItem `json:"items"`
}

type devboxItem struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   devboxMeta   `json:"metadata"`
	Spec       devboxSpec   `json:"spec"`
	Status     devboxStatus `json:"status"`
}

type devboxStatus struct {
	State               string            `json:"state"`
	Phase               string            `json:"phase"`
	Network             any               `json:"network"`
	Conditions          []devboxCondition `json:"conditions"`
	ContentID           string            `json:"contentID"`
	LastContainerStatus any               `json:"lastContainerStatus"`
}

type devboxCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type devboxEventList struct {
	Items []devboxEvent `json:"items"`
}

type devboxEvent struct {
	Type          string `json:"type"`
	Reason        string `json:"reason"`
	Message       string `json:"message"`
	LastTimestamp string `json:"lastTimestamp"`
	EventTime     string `json:"eventTime"`
}

func normalizeDevboxState(item devboxItem) string {
	// spec.state is the requested state, not proof that the controller reached it.
	// In particular, new manifests request Running before status or SSH routing exists.
	for _, value := range []string{item.Status.State, item.Status.Phase} {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "running", "ready":
			return "Running"
		case "pending", "creating", "provisioning", "starting", "scheduling", "scheduled":
			return "Pending"
		case "paused", "pausing":
			return "Paused"
		case "stopped", "stopping":
			return "Stopped"
		case "shutdown", "shuttingdown", "shutting-down":
			return "Shutdown"
		case "error", "failed", "failure", "crashloopbackoff", "unknown":
			return "Error"
		}
	}
	return "Pending"
}

func devboxReady(item devboxItem) bool {
	return normalizeDevboxState(item) == "Running"
}

func devboxTerminalFailure(item devboxItem) bool {
	return normalizeDevboxState(item) == "Error"
}

func devboxSecretName(item devboxItem) string {
	// The Sealos v1alpha2 controller creates the SSH Secret with the DevBox name;
	// status does not publish a separate Secret reference.
	return strings.TrimSpace(item.Metadata.Name)
}

func devboxDiagnostics(item devboxItem, events []devboxEvent, cause error) string {
	parts := []string{}
	if state := strings.TrimSpace(item.Status.State); state != "" {
		parts = append(parts, "state="+redactSensitive(state))
	}
	if phase := strings.TrimSpace(item.Status.Phase); phase != "" {
		parts = append(parts, "phase="+redactSensitive(phase))
	}
	for _, condition := range item.Status.Conditions {
		summary := strings.TrimSpace(condition.Type + "=" + condition.Status)
		if condition.Reason != "" {
			summary += "/" + condition.Reason
		}
		if condition.Message != "" {
			summary += ": " + condition.Message
		}
		parts = append(parts, "condition="+redactSensitive(summary))
	}
	for _, event := range events {
		summary := strings.TrimSpace(event.Type + " " + event.Reason)
		if event.Message != "" {
			summary += ": " + event.Message
		}
		parts = append(parts, "event="+redactSensitive(summary))
	}
	if cause != nil {
		parts = append(parts, "last_error="+redactSensitive(cause.Error()))
	}
	if len(parts) == 0 {
		return "no diagnostics available"
	}
	return strings.Join(parts, "; ")
}

func devboxStatusLabel(item devboxItem) string {
	state := normalizeDevboxState(item)
	if state == "" {
		return "Pending"
	}
	return state
}
