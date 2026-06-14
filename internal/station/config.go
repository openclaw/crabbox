package station

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the parsed `station` config block. Profiles are keyed by name.
type Config struct {
	Profiles map[string]StationProfile
}

// fileConfig mirrors the on-disk YAML shape:
//
//	station:
//	  profiles:
//	    agent:
//	      enabled: true
//	      command: scripts/agent-loop.sh
//	      ttl: 10h
//	      idleTimeout: 45m
//	      restartPolicy: never
type fileConfig struct {
	Profiles map[string]fileStationProfile `yaml:"profiles,omitempty"`
}

type fileStationProfile struct {
	Enabled       bool             `yaml:"enabled,omitempty"`
	Agent         bool             `yaml:"agent,omitempty"`
	Command       string           `yaml:"command,omitempty"`
	TTL           string           `yaml:"ttl,omitempty"`
	IdleTimeout   string           `yaml:"idleTimeout,omitempty"`
	RestartPolicy string           `yaml:"restartPolicy,omitempty"`
	ModelAccess   *fileModelAccess `yaml:"modelAccess,omitempty"`
	// Allow is intentionally absent: model/tool credentials must not flow
	// through env.allow, and station profiles do not carry an env.allow list.
}

type fileModelAccess struct {
	Enabled       bool     `yaml:"enabled,omitempty"`
	Gateway       string   `yaml:"gateway,omitempty"`
	AllowedModels []string `yaml:"allowedModels,omitempty"`
	AllowedTools  []string `yaml:"allowedTools,omitempty"`
	EgressProfile string   `yaml:"egressProfile,omitempty"`
	BudgetUSD     float64  `yaml:"budgetUSD,omitempty"`
}

// Parse decodes a `station` config block from YAML and validates it. The input
// is the YAML value of the `station` key (not the whole config document).
//
// Parsing never enables anything: a profile is disabled unless it explicitly
// sets enabled: true, and modelAccess stays inert regardless of declaration.
func Parse(data []byte) (Config, error) {
	var file fileConfig
	if err := yaml.Unmarshal(data, &file); err != nil {
		return Config{}, fmt.Errorf("station: %w", err)
	}
	cfg := Config{Profiles: map[string]StationProfile{}}
	for _, name := range sortedKeys(file.Profiles) {
		profile, err := convertProfile(name, file.Profiles[name])
		if err != nil {
			return Config{}, err
		}
		cfg.Profiles[name] = profile
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func convertProfile(name string, in fileStationProfile) (StationProfile, error) {
	profile := StationProfile{
		Name:          name,
		Enabled:       in.Enabled,
		Agent:         in.Agent,
		Command:       strings.TrimSpace(in.Command),
		RestartPolicy: RestartNever,
	}
	if rp := strings.TrimSpace(in.RestartPolicy); rp != "" {
		profile.RestartPolicy = RestartPolicy(rp)
	}
	if ttl := strings.TrimSpace(in.TTL); ttl != "" {
		parsed, err := time.ParseDuration(ttl)
		if err != nil {
			return StationProfile{}, fmt.Errorf("station profile %q ttl: %w", name, err)
		}
		profile.TTL = parsed
	}
	if idle := strings.TrimSpace(in.IdleTimeout); idle != "" {
		parsed, err := time.ParseDuration(idle)
		if err != nil {
			return StationProfile{}, fmt.Errorf("station profile %q idleTimeout: %w", name, err)
		}
		profile.IdleTimeout = parsed
	}
	if in.ModelAccess != nil {
		profile.ModelAccess = ModelAccessPolicy{
			Enabled:       in.ModelAccess.Enabled,
			Gateway:       strings.TrimSpace(in.ModelAccess.Gateway),
			AllowedModels: trimAll(in.ModelAccess.AllowedModels),
			AllowedTools:  trimAll(in.ModelAccess.AllowedTools),
			EgressProfile: strings.TrimSpace(in.ModelAccess.EgressProfile),
			BudgetUSD:     in.ModelAccess.BudgetUSD,
		}
	}
	return profile, nil
}

// Validate checks every profile for structural errors. It does not enable any
// phase; that remains the job of Gate.
func (c Config) Validate() error {
	for _, name := range sortedKeys(c.Profiles) {
		if err := c.Profiles[name].Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks a single profile for structural errors.
func (p StationProfile) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("station profile name must not be empty")
	}
	switch p.RestartPolicy {
	case RestartNever:
	case RestartOnFailure:
		// Restarts are a later product decision; reject them for now so a
		// profile cannot silently rely on unimplemented replay behavior.
		return fmt.Errorf("station profile %q restartPolicy %q is not yet supported; only %q is allowed", p.Name, p.RestartPolicy, RestartNever)
	default:
		return fmt.Errorf("station profile %q has unknown restartPolicy %q", p.Name, p.RestartPolicy)
	}
	if p.TTL < 0 {
		return fmt.Errorf("station profile %q ttl must not be negative", p.Name)
	}
	if p.IdleTimeout < 0 {
		return fmt.Errorf("station profile %q idleTimeout must not be negative", p.Name)
	}
	if p.IdleTimeout > 0 && p.TTL > 0 && p.IdleTimeout > p.TTL {
		return fmt.Errorf("station profile %q idleTimeout must not exceed ttl", p.Name)
	}
	if p.Enabled && p.Command == "" {
		return fmt.Errorf("station profile %q is enabled but has no command", p.Name)
	}
	return p.ModelAccess.validate(p.Name)
}

func (m ModelAccessPolicy) validate(profileName string) error {
	if m.IsZero() {
		return nil
	}
	// Enforce the structural pieces of the roadmap's modelAccess security gates
	// at parse time. The runtime gate (Gate) still refuses to deliver
	// credentials until the modelAccess phase is enabled.
	if !m.Enabled {
		return fmt.Errorf("station profile %q modelAccess is configured but not enabled; set enabled: true or remove it", profileName)
	}
	if m.Gateway == "" {
		return fmt.Errorf("station profile %q modelAccess requires a gateway", profileName)
	}
	if len(m.AllowedModels) == 0 && len(m.AllowedTools) == 0 {
		return fmt.Errorf("station profile %q modelAccess must allow at least one model or tool", profileName)
	}
	if m.EgressProfile == "" {
		// Egress defaults to deny; an empty egress profile would mean no egress
		// at all, which is never a valid model-access configuration.
		return fmt.Errorf("station profile %q modelAccess requires a named egressProfile (egress defaults to deny)", profileName)
	}
	if m.BudgetUSD <= 0 {
		return fmt.Errorf("station profile %q modelAccess requires a positive budgetUSD", profileName)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func trimAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
