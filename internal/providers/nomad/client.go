package nomad

import (
	"context"
	"io"
	"os"
	"strings"

	nomadapi "github.com/hashicorp/nomad/api"
)

type Client interface {
	AgentSelf(context.Context) (*nomadapi.AgentSelf, error)
	Regions(context.Context) ([]string, error)
	NamespaceInfo(context.Context, string) (*nomadapi.Namespace, error)
	RegisterJob(context.Context, *nomadapi.Job) (string, error)
	JobInfo(context.Context, string) (*nomadapi.Job, error)
	JobAllocations(context.Context, string, bool) ([]*nomadapi.AllocationListStub, error)
	EvaluationInfo(context.Context, string) (*nomadapi.Evaluation, error)
	DeregisterJob(context.Context, string, bool) (string, error)
	AllocationExec(context.Context, nomadExecRequest) (int, error)
}

type liveClient struct {
	client *nomadapi.Client
	cfg    Config
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
	return liveClient{client: client, cfg: cfg}, nil
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

func (c liveClient) RegisterJob(_ context.Context, job *nomadapi.Job) (string, error) {
	resp, _, err := c.client.Jobs().Register(job, c.writeOptions())
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}
	return resp.EvalID, nil
}

func (c liveClient) JobInfo(_ context.Context, jobID string) (*nomadapi.Job, error) {
	job, _, err := c.client.Jobs().Info(jobID, c.queryOptions())
	return job, err
}

func (c liveClient) JobAllocations(_ context.Context, jobID string, all bool) ([]*nomadapi.AllocationListStub, error) {
	allocs, _, err := c.client.Jobs().Allocations(jobID, all, c.queryOptions())
	return allocs, err
}

func (c liveClient) EvaluationInfo(_ context.Context, evalID string) (*nomadapi.Evaluation, error) {
	eval, _, err := c.client.Evaluations().Info(evalID, c.queryOptions())
	return eval, err
}

func (c liveClient) DeregisterJob(_ context.Context, jobID string, purge bool) (string, error) {
	evalID, _, err := c.client.Jobs().Deregister(jobID, purge, c.writeOptions())
	return evalID, err
}

func (c liveClient) AllocationExec(ctx context.Context, req nomadExecRequest) (int, error) {
	stdin := req.Stdin
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	stdout := req.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := req.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	alloc := &nomadapi.Allocation{
		ID:        req.AllocationID,
		NodeID:    req.NodeID,
		NodeName:  req.NodeName,
		JobID:     req.JobID,
		Namespace: normalizeNamespace(c.cfg.Nomad.Namespace),
	}
	return c.client.Allocations().Exec(ctx, alloc, req.Task, false, req.Command, stdin, stdout, stderr, nil, c.queryOptions())
}

func (c liveClient) queryOptions() *nomadapi.QueryOptions {
	return &nomadapi.QueryOptions{
		Region:    c.cfg.Nomad.Region,
		Namespace: c.cfg.Nomad.Namespace,
	}
}

func (c liveClient) writeOptions() *nomadapi.WriteOptions {
	return &nomadapi.WriteOptions{
		Region:    c.cfg.Nomad.Region,
		Namespace: c.cfg.Nomad.Namespace,
	}
}
