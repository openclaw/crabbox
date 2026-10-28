package cli

import "context"

type ProviderAgentCommander interface {
	RunAgent(context.Context, Config, Runtime, []string) error
}
type serveKongCmd struct {
	Args []string `arg:"" optional:"" passthrough:""`
}

func (c *serveKongCmd) Run(ctx context.Context, app App) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	p, err := ProviderFor("serve")
	if err != nil {
		return err
	}
	command, ok := p.(ProviderAgentCommander)
	if !ok {
		return Exit(2, "serve agent unavailable")
	}
	return command.RunAgent(ctx, cfg, runtimeForApp(app), stripKongCommandPath(c.Args, "serve"))
}

// NewCoordinatorClient shares normal login, transport and redirect protections with agents.
func NewCoordinatorClient(cfg Config) (*CoordinatorClient, error) {
	c, _, err := newCoordinatorClient(cfg)
	return c, err
}
func (c *CoordinatorClient) AgentRequest(ctx context.Context, method, path string, body, out any) error {
	return c.do(ctx, method, path, body, out)
}

func Version() string { return currentVersion() }

type hostKongCmd struct {
	Args []string `arg:"" optional:"" passthrough:""`
}

func (c *hostKongCmd) Run(ctx context.Context, app App) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	p, err := ProviderFor("ssh-host")
	if err != nil {
		return err
	}
	command, ok := p.(ProviderAgentCommander)
	if !ok {
		return Exit(2, "managed host agent unavailable")
	}
	return command.RunAgent(ctx, cfg, runtimeForApp(app), stripKongCommandPath(c.Args, "host"))
}
