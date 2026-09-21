package cli

import "flag"

// Nil selectors are absent from that command's fixed flag surface.
type actionsWorkflowFlagValues struct {
	Repo     *string
	Workflow *string
	Job      *string
	Ref      *string
}

func registerActionsRepoFlag(fs *flag.FlagSet) *string {
	return fs.String("repo", "", "GitHub repository owner/name")
}
func registerActionsWorkflowFlag(fs *flag.FlagSet) *string {
	return fs.String("workflow", "", "workflow file/name/id")
}
func registerActionsRefFlag(fs *flag.FlagSet) *string {
	return fs.String("ref", "", "workflow ref")
}

func registerActionsHydrateWorkflowFlags(fs *flag.FlagSet) actionsWorkflowFlagValues {
	return actionsWorkflowFlagValues{
		Repo:     registerActionsRepoFlag(fs),
		Workflow: registerActionsWorkflowFlag(fs),
		Job:      fs.String("job", "", "expected hydrate workflow job/input name"),
		Ref:      registerActionsRefFlag(fs),
	}
}
func registerActionsDispatchWorkflowFlags(fs *flag.FlagSet) actionsWorkflowFlagValues {
	return actionsWorkflowFlagValues{
		Repo:     registerActionsRepoFlag(fs),
		Workflow: registerActionsWorkflowFlag(fs),
		Ref:      registerActionsRefFlag(fs),
	}
}
func registerActionsRepositoryFlags(fs *flag.FlagSet) actionsWorkflowFlagValues {
	return actionsWorkflowFlagValues{Repo: registerActionsRepoFlag(fs)}
}

func (values actionsWorkflowFlagValues) Apply(cfg *Config) {
	if values.Repo != nil && *values.Repo != "" {
		cfg.Actions.Repo = *values.Repo
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
	}
	if values.Workflow != nil && *values.Workflow != "" {
		cfg.Actions.Workflow = *values.Workflow
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
	}
	if values.Job != nil && *values.Job != "" {
		cfg.Actions.Job = *values.Job
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
	}
	if values.Ref != nil && *values.Ref != "" {
		cfg.Actions.Ref = *values.Ref
		recordConfigInput(cfg, configInputGeneric, configInputFlag, true)
	}
}

// Parsed fields stay command-local: hydration merges them; dispatch forwards them.
func registerActionsInputFields(fs *flag.FlagSet) *stringListFlag {
	fields := stringListFlag{}
	fs.Var(&fields, "f", "workflow input key=value")
	fs.Var(&fields, "field", "workflow input key=value")
	return &fields
}
