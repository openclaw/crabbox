package cli

import "os"

// ActionsWorkflowConfig is the shared workflow selection and configured-input record.
type ActionsWorkflowConfig struct {
	Repo     string   `yaml:"repo,omitempty"`
	Workflow string   `yaml:"workflow,omitempty"`
	Job      string   `yaml:"job,omitempty"`
	Ref      string   `yaml:"ref,omitempty"`
	Fields   []string `yaml:"fields,omitempty"`
}

type ActionsConfig struct {
	ActionsWorkflowConfig `yaml:",inline"`
	RunnerLabels          []string
	RunnerVersion         string
	Ephemeral             bool
}

type JobActionsConfig ActionsWorkflowConfig

type fileActionsConfig struct {
	ActionsWorkflowConfig `yaml:",inline"`
	RunnerLabels          []string `yaml:"runnerLabels,omitempty"`
	RunnerVersion         string   `yaml:"runnerVersion,omitempty"`
	Ephemeral             *bool    `yaml:"ephemeral,omitempty"`
}

type fileJobActionsConfig ActionsWorkflowConfig

func initialActionsConfig() ActionsConfig {
	return ActionsConfig{RunnerVersion: "latest", Ephemeral: true}
}

func (cfg *ActionsWorkflowConfig) applyFile(file *ActionsWorkflowConfig) bool {
	if file == nil {
		return false
	}
	accepted := false
	if file.Repo != "" {
		cfg.Repo = file.Repo
		accepted = true
	}
	if file.Workflow != "" {
		cfg.Workflow = file.Workflow
		accepted = true
	}
	if file.Job != "" {
		cfg.Job = file.Job
		accepted = true
	}
	if file.Ref != "" {
		cfg.Ref = file.Ref
		accepted = true
	}
	if applyActionsFileList(&cfg.Fields, file.Fields) {
		accepted = true
	}
	return accepted
}

func applyActionsFileList(dst *[]string, values []string) bool {
	if len(values) == 0 {
		return false
	}
	// A nonempty all-blank input still clears the value and counts as accepted.
	*dst = appendUniqueStrings(nil, values...)
	return true
}

func applyActionsFileConfig(cfg *Config, file *fileActionsConfig, source configInputSource) {
	if file == nil {
		return
	}
	recordConfigInput(cfg, configInputGeneric, source, cfg.Actions.ActionsWorkflowConfig.applyFile(&file.ActionsWorkflowConfig))
	recordConfigInput(cfg, configInputGeneric, source, applyActionsFileList(&cfg.Actions.RunnerLabels, file.RunnerLabels))
	if file.RunnerVersion != "" {
		cfg.Actions.RunnerVersion = file.RunnerVersion
		recordConfigInput(cfg, configInputGeneric, source, true)
	}
	recordConfigInput(cfg, configInputGeneric, source, applyOptional(&cfg.Actions.Ephemeral, file.Ephemeral))
}

func (cfg *JobActionsConfig) applyFile(file *fileJobActionsConfig) {
	(*ActionsWorkflowConfig)(cfg).applyFile((*ActionsWorkflowConfig)(file))
}

func applyActionsEnvPrefix(cfg *Config) {
	cfg.Actions.Workflow = configInputEnvString(cfg, configInputGeneric, cfg.Actions.Workflow, "CRABBOX_ACTIONS_WORKFLOW")
	cfg.Actions.Job = configInputEnvString(cfg, configInputGeneric, cfg.Actions.Job, "CRABBOX_ACTIONS_JOB")
	cfg.Actions.Ref = configInputEnvString(cfg, configInputGeneric, cfg.Actions.Ref, "CRABBOX_ACTIONS_REF")
	cfg.Actions.Repo = configInputEnvString(cfg, configInputGeneric, cfg.Actions.Repo, "CRABBOX_ACTIONS_REPO")
	cfg.Actions.RunnerVersion = configInputEnvString(cfg, configInputGeneric, cfg.Actions.RunnerVersion, "CRABBOX_ACTIONS_RUNNER_VERSION")
}

func applyActionsEnvSuffix(cfg *Config) {
	if labels := os.Getenv("CRABBOX_ACTIONS_RUNNER_LABELS"); labels != "" {
		cfg.Actions.RunnerLabels = splitCommaList(labels)
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
	if value, ok := getenvBool("CRABBOX_ACTIONS_EPHEMERAL"); ok {
		cfg.Actions.Ephemeral = value
		recordConfigInput(cfg, configInputGeneric, configInputEnvironment, true)
	}
}
