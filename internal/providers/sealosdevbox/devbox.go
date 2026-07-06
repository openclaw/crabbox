package sealosdevbox

import (
	"fmt"
	"sort"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"gopkg.in/yaml.v3"
)

const (
	devboxKind              = "Devbox"
	devboxStateRun          = "Running"
	devboxRuntimeClass      = "devbox-runtime"
	devboxSchedulingNodeKey = "devbox.sealos.io/node"
	devboxSSHPortName       = "devbox-ssh-port"

	managedByLabel     = "app.kubernetes.io/managed-by"
	providerLabel      = "crabbox.dev/provider"
	leaseIDLabel       = "crabbox.dev/lease-id"
	slugLabel          = "crabbox.dev/slug"
	providerScopeLabel = "crabbox.dev/provider-scope"
	annotationBase     = "crabbox.dev/"
)

type devboxManifest struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Metadata   devboxMeta `yaml:"metadata"`
	Spec       devboxSpec `yaml:"spec"`
}

type devboxMeta struct {
	Name              string            `yaml:"name" json:"name"`
	Namespace         string            `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	UID               string            `yaml:"-" json:"uid,omitempty"`
	ResourceVersion   string            `yaml:"-" json:"resourceVersion,omitempty"`
	CreationTimestamp string            `yaml:"-" json:"creationTimestamp,omitempty"`
	Labels            map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	Annotations       map[string]string `yaml:"annotations,omitempty" json:"annotations,omitempty"`
	OwnerReferences   []ownerReference  `yaml:"-" json:"ownerReferences,omitempty"`
}

type ownerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

type devboxSpec struct {
	State                  string             `yaml:"state,omitempty" json:"state,omitempty"`
	Resource               devboxResourceSpec `yaml:"resource,omitempty" json:"resource,omitempty"`
	Image                  string             `yaml:"image,omitempty" json:"image,omitempty"`
	TemplateID             string             `yaml:"templateID,omitempty" json:"templateID,omitempty"`
	MergeBaseImageTopLayer bool               `yaml:"mergeBaseImageTopLayer" json:"mergeBaseImageTopLayer"`
	Config                 devboxConfigSpec   `yaml:"config,omitempty" json:"config,omitempty"`
	RuntimeClassName       string             `yaml:"runtimeClassName,omitempty" json:"runtimeClassName,omitempty"`
	StorageLimit           string             `yaml:"storageLimit,omitempty" json:"storageLimit,omitempty"`
	Network                devboxNetworkSpec  `yaml:"network,omitempty" json:"network,omitempty"`
	Tolerations            []devboxToleration `yaml:"tolerations,omitempty" json:"tolerations,omitempty"`
	Affinity               devboxAffinity     `yaml:"affinity,omitempty" json:"affinity,omitempty"`
}

type devboxResourceSpec struct {
	CPU              string `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	Memory           string `yaml:"memory,omitempty" json:"memory,omitempty"`
	EphemeralStorage string `yaml:"ephemeral-storage,omitempty" json:"ephemeral-storage,omitempty"`
}

type devboxConfigSpec struct {
	User       string            `yaml:"user,omitempty" json:"user,omitempty"`
	WorkingDir string            `yaml:"workingDir,omitempty" json:"workingDir,omitempty"`
	Ports      []devboxPortSpec  `yaml:"ports,omitempty" json:"ports,omitempty"`
	Env        map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
}

type devboxPortSpec struct {
	Name          string `yaml:"name,omitempty" json:"name,omitempty"`
	ContainerPort int    `yaml:"containerPort,omitempty" json:"containerPort,omitempty"`
	Protocol      string `yaml:"protocol,omitempty" json:"protocol,omitempty"`
}

type devboxNetworkSpec struct {
	Type string `yaml:"type,omitempty" json:"type,omitempty"`
}

type devboxToleration struct {
	Key      string `yaml:"key" json:"key"`
	Operator string `yaml:"operator" json:"operator"`
	Effect   string `yaml:"effect" json:"effect"`
}

type devboxAffinity struct {
	NodeAffinity devboxNodeAffinity `yaml:"nodeAffinity" json:"nodeAffinity"`
}

type devboxNodeAffinity struct {
	Required devboxNodeSelector `yaml:"requiredDuringSchedulingIgnoredDuringExecution" json:"requiredDuringSchedulingIgnoredDuringExecution"`
}

type devboxNodeSelector struct {
	Terms []devboxNodeSelectorTerm `yaml:"nodeSelectorTerms" json:"nodeSelectorTerms"`
}

type devboxNodeSelectorTerm struct {
	Expressions []devboxNodeSelectorRequirement `yaml:"matchExpressions" json:"matchExpressions"`
}

type devboxNodeSelectorRequirement struct {
	Key      string `yaml:"key" json:"key"`
	Operator string `yaml:"operator" json:"operator"`
}

func (b *backend) renderDevboxManifest(name, leaseID, slug string, keep bool, now time.Time) ([]byte, error) {
	cfg := b.cfg.SealosDevbox
	if strings.TrimSpace(cfg.Image) == "" {
		return nil, core.Exit(2, "sealos-devbox requires image to create a DevBox")
	}
	if strings.TrimSpace(cfg.CPU) == "" || strings.TrimSpace(cfg.Memory) == "" || strings.TrimSpace(cfg.StorageLimit) == "" {
		return nil, core.Exit(2, "sealos-devbox cpu, memory, and storageLimit are required")
	}
	network := normalizeNetwork(cfg.Network)
	if network != networkSSHGate && network != networkNodePort {
		return nil, core.Exit(2, "sealos-devbox network must be SSHGate or NodePort")
	}
	annotations := b.devboxAnnotations(name, leaseID, slug, keep, now)
	doc := devboxManifest{
		APIVersion: devboxGroupVersion,
		Kind:       devboxKind,
		Metadata: devboxMeta{
			Name:        name,
			Namespace:   cfg.Namespace,
			Labels:      b.devboxLabels(leaseID, slug),
			Annotations: annotations,
		},
		Spec: devboxSpec{
			State:                  devboxStateRun,
			MergeBaseImageTopLayer: true,
			RuntimeClassName:       devboxRuntimeClass,
			Resource: devboxResourceSpec{
				CPU:              strings.TrimSpace(cfg.CPU),
				Memory:           strings.TrimSpace(cfg.Memory),
				EphemeralStorage: strings.TrimSpace(cfg.StorageLimit),
			},
			Image:        strings.TrimSpace(cfg.Image),
			TemplateID:   strings.TrimSpace(cfg.TemplateID),
			StorageLimit: strings.TrimSpace(cfg.StorageLimit),
			Network:      devboxNetworkSpec{Type: network},
			Config: devboxConfigSpec{
				User:       strings.TrimSpace(cfg.SSHUser),
				WorkingDir: sealosWorkRoot(b.cfg),
				Ports:      []devboxPortSpec{{Name: devboxSSHPortName, ContainerPort: 22, Protocol: "TCP"}},
			},
			Tolerations: []devboxToleration{{
				Key:      devboxSchedulingNodeKey,
				Operator: "Exists",
				Effect:   "NoSchedule",
			}},
			Affinity: devboxAffinity{
				NodeAffinity: devboxNodeAffinity{
					Required: devboxNodeSelector{
						Terms: []devboxNodeSelectorTerm{{
							Expressions: []devboxNodeSelectorRequirement{{
								Key:      devboxSchedulingNodeKey,
								Operator: "Exists",
							}},
						}},
					},
				},
			},
		},
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("encode Sealos Devbox manifest: %w", err)
	}
	return out, nil
}

func (b *backend) devboxLabels(leaseID, slug string) map[string]string {
	return map[string]string{
		managedByLabel:     "crabbox",
		providerLabel:      providerName,
		leaseIDLabel:       strings.TrimSpace(leaseID),
		slugLabel:          core.NormalizeLeaseSlug(slug),
		providerScopeLabel: b.claimScopeLabel(),
	}
}

func (b *backend) devboxAnnotations(name, leaseID, slug string, keep bool, now time.Time) map[string]string {
	labels := core.DirectLeaseLabels(b.cfg, leaseID, slug, providerName, "", keep, now)
	labels["devbox_namespace"] = b.cfg.SealosDevbox.Namespace
	labels["devbox_name"] = name
	labels["network"] = normalizeNetwork(b.cfg.SealosDevbox.Network)
	labels["provider-scope"] = b.claimScopeID()
	labels["provider_scope_id"] = b.claimScopeID()
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(labels))
	for _, key := range keys {
		out[annotationBase+key] = labels[key]
	}
	return out
}
