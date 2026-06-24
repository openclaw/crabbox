package station

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestParseAgentProfile(t *testing.T) {
	cfg, err := Parse([]byte(`
profiles:
  agent:
    enabled: true
    agent: true
    command: scripts/agent-loop.sh
    ttl: 10h
    idleTimeout: 45m
    restartPolicy: never
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	profile, ok := cfg.Profiles["agent"]
	if !ok {
		t.Fatalf("agent profile missing: %#v", cfg.Profiles)
	}
	if !profile.Enabled || !profile.Agent {
		t.Fatalf("expected enabled agent profile, got %#v", profile)
	}
	if profile.Command != "scripts/agent-loop.sh" {
		t.Fatalf("command = %q", profile.Command)
	}
	if profile.TTL != 10*time.Hour {
		t.Fatalf("ttl = %v", profile.TTL)
	}
	if profile.IdleTimeout != 45*time.Minute {
		t.Fatalf("idleTimeout = %v", profile.IdleTimeout)
	}
	if profile.RestartPolicy != RestartNever {
		t.Fatalf("restartPolicy = %q", profile.RestartPolicy)
	}
}

func TestRoadmapAgentProfileExampleIsAdmitted(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "features", "station-profiles.md"))
	if err != nil {
		t.Fatalf("read roadmap: %v", err)
	}
	sample, ok := fencedBlockAfter(string(doc), "## Agent Profile", "```yaml")
	if !ok {
		t.Fatalf("agent profile YAML sample missing from roadmap")
	}
	cfg, err := parseDocumentedStationSample(sample)
	if err != nil {
		t.Fatalf("Parse documented sample: %v", err)
	}
	profile, ok := cfg.Profiles["agent"]
	if !ok {
		t.Fatalf("documented sample did not define agent profile: %#v", cfg.Profiles)
	}
	if _, err := NewAgentProfile(DefaultGate().WithPhase(PhaseAgentProfile), profile); err != nil {
		t.Fatalf("documented sample should be admitted as agent profile: %v", err)
	}
}

func TestParseDefaultsDisabledAndRestartNever(t *testing.T) {
	cfg, err := Parse([]byte(`
profiles:
  default: {}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	profile := cfg.Profiles["default"]
	if profile.Enabled {
		t.Fatalf("profile should be disabled by default: %#v", profile)
	}
	if profile.RestartPolicy != RestartNever {
		t.Fatalf("restartPolicy should default to never, got %q", profile.RestartPolicy)
	}
	if !profile.ModelAccess.IsZero() {
		t.Fatalf("modelAccess should be empty by default: %#v", profile.ModelAccess)
	}
}

func TestParseRejectsBadDuration(t *testing.T) {
	_, err := Parse([]byte(`
profiles:
  agent:
    enabled: true
    command: scripts/agent-loop.sh
    ttl: notaduration
`))
	if err == nil || !strings.Contains(err.Error(), "ttl") {
		t.Fatalf("expected ttl parse error, got %v", err)
	}
}

func TestValidateRejectsEnabledWithoutCommand(t *testing.T) {
	_, err := Parse([]byte(`
profiles:
  agent:
    enabled: true
`))
	if err == nil || !strings.Contains(err.Error(), "no command") {
		t.Fatalf("expected missing-command error, got %v", err)
	}
}

func TestValidateRejectsIdleExceedingTTL(t *testing.T) {
	_, err := Parse([]byte(`
profiles:
  agent:
    enabled: true
    command: scripts/agent-loop.sh
    ttl: 10m
    idleTimeout: 1h
`))
	if err == nil || !strings.Contains(err.Error(), "idleTimeout") {
		t.Fatalf("expected idleTimeout > ttl error, got %v", err)
	}
}

func TestValidateRejectsUnsupportedRestartPolicy(t *testing.T) {
	_, err := Parse([]byte(`
profiles:
  agent:
    enabled: true
    command: scripts/agent-loop.sh
    restartPolicy: on-failure
`))
	if err == nil || !strings.Contains(err.Error(), "restartPolicy") {
		t.Fatalf("expected restartPolicy error, got %v", err)
	}
}

func TestModelAccessIsSeparateFromEnvAllow(t *testing.T) {
	// The station profile YAML shape has no env.allow field at all: declaring
	// model access there must not be possible. Parsing a profile with an
	// `envAllow` key should simply ignore it (it is not part of the schema),
	// proving modelAccess cannot ride in on env.allow.
	cfg, err := Parse([]byte(`
profiles:
  agent:
    enabled: true
    command: scripts/agent-loop.sh
    envAllow:
      - OPENAI_API_KEY
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.Profiles["agent"].ModelAccess.IsZero() {
		t.Fatalf("env.allow must never populate modelAccess: %#v", cfg.Profiles["agent"].ModelAccess)
	}
}

func TestModelAccessValidation(t *testing.T) {
	// A fully-specified, enabled modelAccess policy parses.
	cfg, err := Parse([]byte(`
profiles:
  agent:
    enabled: true
    agent: true
    command: scripts/agent-loop.sh
    ttl: 10h
    modelAccess:
      enabled: true
      gateway: scoped-gateway
      allowedModels:
        - claude-opus-4-8
      egressProfile: approved-anthropic
      budgetUSD: 5
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ma := cfg.Profiles["agent"].ModelAccess
	if !ma.Enabled || ma.Gateway != "scoped-gateway" || ma.BudgetUSD != 5 {
		t.Fatalf("unexpected modelAccess: %#v", ma)
	}

	// modelAccess declared but missing required fields is rejected.
	cases := map[string]string{
		"missing gateway": `
profiles:
  a:
    enabled: true
    command: c
    modelAccess:
      enabled: true
      allowedModels: [m]
      egressProfile: e
      budgetUSD: 1
`,
		"no models or tools": `
profiles:
  a:
    enabled: true
    command: c
    modelAccess:
      enabled: true
      gateway: g
      egressProfile: e
      budgetUSD: 1
`,
		"missing egress profile": `
profiles:
  a:
    enabled: true
    command: c
    modelAccess:
      enabled: true
      gateway: g
      allowedModels: [m]
      budgetUSD: 1
`,
		"non-positive budget": `
profiles:
  a:
    enabled: true
    command: c
    modelAccess:
      enabled: true
      gateway: g
      allowedModels: [m]
      egressProfile: e
`,
		"configured but disabled": `
profiles:
  a:
    enabled: true
    command: c
    modelAccess:
      gateway: g
      allowedModels: [m]
      egressProfile: e
      budgetUSD: 1
`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func fencedBlockAfter(doc, heading, fence string) (string, bool) {
	headingAt := strings.Index(doc, heading)
	if headingAt < 0 {
		return "", false
	}
	afterHeading := doc[headingAt+len(heading):]
	fenceAt := strings.Index(afterHeading, fence)
	if fenceAt < 0 {
		return "", false
	}
	afterFence := afterHeading[fenceAt+len(fence):]
	endAt := strings.Index(afterFence, "```")
	if endAt < 0 {
		return "", false
	}
	return strings.TrimSpace(afterFence[:endAt]), true
}

func parseDocumentedStationSample(sample string) (Config, error) {
	var root struct {
		Station yaml.Node `yaml:"station"`
	}
	if err := yaml.Unmarshal([]byte(sample), &root); err != nil {
		return Config{}, err
	}
	if root.Station.Kind == 0 {
		return Parse([]byte(sample))
	}
	stationBlock, err := yaml.Marshal(&root.Station)
	if err != nil {
		return Config{}, err
	}
	return Parse(stationBlock)
}
