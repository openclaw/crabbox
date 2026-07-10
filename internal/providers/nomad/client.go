package nomad

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/go-cleanhttp"
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

var (
	errNomadCrossOriginRedirect = errors.New("nomad refused cross-origin redirect")
	errNomadInvalidRedirect     = errors.New("nomad refused invalid redirect")
	errNomadRedirectLimit       = errors.New("nomad redirect stopped after 10 redirects")
)

func newNomadClient(cfg Config, rt Runtime) (Client, error) {
	apiConfig, err := newNomadAPIConfig(cfg, os.Getenv)
	if err != nil {
		return nil, err
	}
	if err := configureNomadHTTPClient(apiConfig, rt.HTTP); err != nil {
		return nil, err
	}
	client, err := nomadapi.NewClient(apiConfig)
	if err != nil {
		return nil, err
	}
	return liveClient{client: client, cfg: cfg}, nil
}

func configureNomadHTTPClient(apiConfig *nomadapi.Config, source *http.Client) error {
	trusted, err := url.Parse(apiConfig.Address)
	if err != nil {
		return err
	}
	if source == nil {
		var transport *http.Transport
		if trusted.Scheme == "unix" {
			socketPath := trusted.EscapedPath()
			dialer := &net.Dialer{}
			transport = &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socketPath)
				},
			}
			source = &http.Client{Transport: transport}
		} else {
			source = cleanhttp.DefaultPooledClient()
			transport = source.Transport.(*http.Transport)
		}
		transport.TLSHandshakeTimeout = 10 * time.Second
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		transport.ForceAttemptHTTP2 = false
		if trusted.Scheme != "unix" {
			if err := nomadapi.ConfigureTLS(source, apiConfig.TLSConfig); err != nil {
				return err
			}
		}
	}
	if trusted.Scheme == "unix" {
		// The SDK rewrites Unix-socket request URLs to this origin while the
		// transport continues to dial only the configured socket.
		trusted = &url.URL{Scheme: "http", Host: "127.0.0.1"}
	}
	apiConfig.HttpClient = secureNomadHTTPClient(source, trusted)
	return nil
}

func secureNomadHTTPClient(source *http.Client, trusted *url.URL) *http.Client {
	client := *source
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameNomadOrigin(trusted, req.URL) {
			return errNomadCrossOriginRedirect
		}
		if originalCheckRedirect != nil {
			return originalCheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return errNomadRedirectLimit
		}
		return nil
	}
	return &client
}

func sameNomadOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveNomadPort(a) == effectiveNomadPort(b)
}

func effectiveNomadPort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	switch strings.ToLower(value.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

func sanitizeNomadClientError(err error) error {
	// net/http wraps redirect errors with the untrusted Location URL. Return
	// only the stable sentinel so provider diagnostics cannot retain it.
	if errors.Is(err, errNomadCrossOriginRedirect) {
		return errNomadCrossOriginRedirect
	}
	if errors.Is(err, errNomadRedirectLimit) {
		return errNomadRedirectLimit
	}
	var requestErr *url.Error
	if errors.As(err, &requestErr) && strings.Contains(requestErr.Err.Error(), "failed to parse Location header") {
		return errNomadInvalidRedirect
	}
	if err != nil && strings.Contains(err.Error(), "invalid redirect location") {
		return errNomadInvalidRedirect
	}
	return err
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

func (c liveClient) AgentSelf(ctx context.Context) (*nomadapi.AgentSelf, error) {
	var value nomadapi.AgentSelf
	query := (&nomadapi.QueryOptions{}).WithContext(ctx)
	if _, err := c.client.Raw().Query("/v1/agent/self", &value, query); err != nil {
		return nil, fmt.Errorf("failed querying self endpoint: %w", sanitizeNomadClientError(err))
	}
	return &value, nil
}

func (c liveClient) Regions(context.Context) ([]string, error) {
	value, err := c.client.Regions().List()
	return value, sanitizeNomadClientError(err)
}

func (c liveClient) NamespaceInfo(ctx context.Context, namespace string) (*nomadapi.Namespace, error) {
	ns, _, err := c.client.Namespaces().Info(namespace, c.queryOptions(ctx))
	return ns, sanitizeNomadClientError(err)
}

func (c liveClient) RegisterJob(ctx context.Context, job *nomadapi.Job) (string, error) {
	resp, _, err := c.client.Jobs().RegisterOpts(job, &nomadapi.RegisterOptions{
		EnforceIndex: true,
		ModifyIndex:  0,
	}, c.writeOptions(ctx))
	if err != nil {
		return "", sanitizeNomadClientError(err)
	}
	if resp == nil {
		return "", nil
	}
	return resp.EvalID, nil
}

func (c liveClient) JobInfo(ctx context.Context, jobID string) (*nomadapi.Job, error) {
	job, _, err := c.client.Jobs().Info(jobID, c.queryOptions(ctx))
	return job, sanitizeNomadClientError(err)
}

func (c liveClient) JobAllocations(ctx context.Context, jobID string, all bool) ([]*nomadapi.AllocationListStub, error) {
	allocs, _, err := c.client.Jobs().Allocations(jobID, all, c.queryOptions(ctx))
	return allocs, sanitizeNomadClientError(err)
}

func (c liveClient) EvaluationInfo(ctx context.Context, evalID string) (*nomadapi.Evaluation, error) {
	eval, _, err := c.client.Evaluations().Info(evalID, c.queryOptions(ctx))
	return eval, sanitizeNomadClientError(err)
}

func (c liveClient) DeregisterJob(ctx context.Context, jobID string, purge bool) (string, error) {
	evalID, _, err := c.client.Jobs().Deregister(jobID, purge, c.writeOptions(ctx))
	return evalID, sanitizeNomadClientError(err)
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
	exitCode, err := c.client.Allocations().Exec(ctx, alloc, req.Task, false, req.Command, stdin, stdout, stderr, nil, c.queryOptions(ctx))
	return exitCode, sanitizeNomadClientError(err)
}

func (c liveClient) queryOptions(ctx context.Context) *nomadapi.QueryOptions {
	opts := &nomadapi.QueryOptions{
		Region:    c.cfg.Nomad.Region,
		Namespace: c.cfg.Nomad.Namespace,
	}
	return opts.WithContext(ctx)
}

func (c liveClient) writeOptions(ctx context.Context) *nomadapi.WriteOptions {
	opts := &nomadapi.WriteOptions{
		Region:    c.cfg.Nomad.Region,
		Namespace: c.cfg.Nomad.Namespace,
	}
	return opts.WithContext(ctx)
}
