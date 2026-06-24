package nomad

import (
	"context"
	"os"

	nomadapi "github.com/hashicorp/nomad/api"
)

type Client interface {
	AgentSelf(context.Context) (*nomadapi.AgentSelf, error)
	Regions(context.Context) ([]string, error)
	NamespaceInfo(context.Context, string) (*nomadapi.Namespace, error)
}

type liveClient struct {
	client *nomadapi.Client
}

func newNomadClient(cfg Config, rt Runtime) (Client, error) {
	apiConfig, err := newNomadAPIConfig(cfg, os.Getenv)
	if err != nil {
		return nil, err
	}
	if rt.HTTP != nil {
		apiConfig.HttpClient = rt.HTTP
	}
	client, err := nomadapi.NewClient(apiConfig)
	if err != nil {
		return nil, err
	}
	return liveClient{client: client}, nil
}

func newNomadAPIConfig(cfg Config, lookup func(string) string) (*nomadapi.Config, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.Nomad.Address == "" {
		return nil, exit(2, "nomad address is required; set NOMAD_ADDR or nomad.address")
	}
	token, _ := nomadToken(cfg, lookup)
	return &nomadapi.Config{
		Address:   cfg.Nomad.Address,
		Region:    cfg.Nomad.Region,
		Namespace: cfg.Nomad.Namespace,
		SecretID:  token,
		TLSConfig: &nomadapi.TLSConfig{
			CACert:        cfg.Nomad.CACert,
			CAPath:        cfg.Nomad.CAPath,
			ClientCert:    cfg.Nomad.ClientCert,
			ClientKey:     cfg.Nomad.ClientKey,
			TLSServerName: cfg.Nomad.TLSServerName,
			Insecure:      cfg.Nomad.SkipVerify,
		},
	}, nil
}

func (c liveClient) AgentSelf(context.Context) (*nomadapi.AgentSelf, error) {
	return c.client.Agent().Self()
}

func (c liveClient) Regions(context.Context) ([]string, error) {
	return c.client.Regions().List()
}

func (c liveClient) NamespaceInfo(_ context.Context, namespace string) (*nomadapi.Namespace, error) {
	ns, _, err := c.client.Namespaces().Info(namespace, nil)
	return ns, err
}
