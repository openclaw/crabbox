package replicate

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type ReplicateConfig = core.ReplicateConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend

const (
	providerName              = "replicate"
	defaultAPIURL             = "https://api.replicate.com/v1"
	defaultWorkdir            = "/workspace/crabbox"
	defaultWaitSecs           = 0
	defaultPollIntervalSecs   = 2
	defaultExecTimeoutSecs    = 3600
	defaultCancelAfterSecs    = 0
	defaultMaxArchiveBytes    = 10 * 1024 * 1024
	envCrabboxReplicateToken  = "CRABBOX_REPLICATE_API_TOKEN"
	envReplicateToken         = "REPLICATE_API_TOKEN"
	runnerExitCodeJSONName    = "exit_code"
	runnerExitCodeAltJSONName = "exitCode"
)

type RunnerInput struct {
	Command      []string          `json:"command"`
	Workdir      string            `json:"workdir"`
	ArchiveURL   string            `json:"archive_url,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	TimeoutSecs  int               `json:"timeout_secs,omitempty"`
	CancelAfter  int               `json:"cancel_after_secs,omitempty"`
	MaxLogBytes  int               `json:"max_log_bytes,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	OutputSchema string            `json:"output_schema,omitempty"`
}

type RunnerOutput struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

func DefaultConfig() ReplicateConfig {
	return ReplicateConfig{
		APIURL:           defaultAPIURL,
		Workdir:          defaultWorkdir,
		WaitSecs:         defaultWaitSecs,
		PollIntervalSecs: defaultPollIntervalSecs,
		ExecTimeoutSecs:  defaultExecTimeoutSecs,
		CancelAfterSecs:  defaultCancelAfterSecs,
		MaxArchiveBytes:  defaultMaxArchiveBytes,
	}
}

func ResolveAPIToken() (string, string, bool) {
	if token := strings.TrimSpace(os.Getenv(envCrabboxReplicateToken)); token != "" {
		return token, envCrabboxReplicateToken, true
	}
	if token := strings.TrimSpace(os.Getenv(envReplicateToken)); token != "" {
		return token, envReplicateToken, true
	}
	return "", "", false
}

func ValidateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.Provider) != providerName {
		return nil
	}
	deployment := strings.TrimSpace(cfg.Replicate.Deployment)
	version := strings.TrimSpace(cfg.Replicate.Version)
	if deployment == "" && version == "" {
		return core.Exit(2, "provider=replicate requires exactly one of replicate.deployment or replicate.version")
	}
	if deployment != "" && version != "" {
		return core.Exit(2, "provider=replicate accepts exactly one of replicate.deployment or replicate.version, not both")
	}
	if cfg.Replicate.WaitSecs < 0 {
		return core.Exit(2, "replicate waitSecs must be non-negative")
	}
	if cfg.Replicate.PollIntervalSecs < 0 {
		return core.Exit(2, "replicate pollIntervalSecs must be non-negative")
	}
	if cfg.Replicate.ExecTimeoutSecs < 0 {
		return core.Exit(2, "replicate execTimeoutSecs must be non-negative")
	}
	if cfg.Replicate.CancelAfterSecs < 0 {
		return core.Exit(2, "replicate cancelAfterSecs must be non-negative")
	}
	if cfg.Replicate.MaxArchiveBytes < 0 {
		return core.Exit(2, "replicate maxArchiveBytes must be non-negative")
	}
	return nil
}

func ParseRunnerOutput(data []byte) (RunnerOutput, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return RunnerOutput{}, fmt.Errorf("decode replicate runner output: %w", err)
	}
	exitRaw, ok := raw[runnerExitCodeJSONName]
	if !ok {
		exitRaw, ok = raw[runnerExitCodeAltJSONName]
	}
	if !ok {
		return RunnerOutput{}, fmt.Errorf("replicate runner output missing required exit_code")
	}
	var exitCode int
	if err := json.Unmarshal(exitRaw, &exitCode); err != nil {
		return RunnerOutput{}, fmt.Errorf("replicate runner output exit_code must be an integer: %w", err)
	}
	var out RunnerOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return RunnerOutput{}, fmt.Errorf("decode replicate runner output fields: %w", err)
	}
	out.ExitCode = exitCode
	return out, nil
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}
