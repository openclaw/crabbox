package awslambdamicrovm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/aws/smithy-go"
)

type microVM struct {
	ID           string
	Endpoint     string
	ImageARN     string
	ImageVersion string
	State        string
	StateReason  string
	StartedAt    time.Time
}

type runMicroVMRequest struct {
	ImageARN          string
	ImageVersion      string
	ExecutionRoleARN  string
	ClientToken       string
	IngressConnectors []string
	EgressConnectors  []string
	IdleSeconds       int32
	SuspendedSeconds  int32
	MaximumSeconds    int32
}

type controlPlane interface {
	Run(context.Context, runMicroVMRequest) (microVM, error)
	Get(context.Context, string) (microVM, error)
	Probe(context.Context, string, string) error
	Terminate(context.Context, string) error
	Suspend(context.Context, string) error
	Resume(context.Context, string) error
	AuthToken(context.Context, string) (string, error)
}

type sdkControlPlane struct{ client *lambdamicrovms.Client }

func newControlPlane(ctx context.Context, cfg Config) (controlPlane, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		return nil, err
	}
	return &sdkControlPlane{client: lambdamicrovms.NewFromConfig(awsCfg)}, nil
}

func (c *sdkControlPlane) Run(ctx context.Context, req runMicroVMRequest) (microVM, error) {
	input := &lambdamicrovms.RunMicrovmInput{
		ImageIdentifier:          aws.String(req.ImageARN),
		ClientToken:              aws.String(req.ClientToken),
		IngressNetworkConnectors: append([]string(nil), req.IngressConnectors...),
		EgressNetworkConnectors:  append([]string(nil), req.EgressConnectors...),
	}
	if req.ImageVersion != "" {
		input.ImageVersion = aws.String(req.ImageVersion)
	}
	if req.ExecutionRoleARN != "" {
		input.ExecutionRoleArn = aws.String(req.ExecutionRoleARN)
	}
	if req.MaximumSeconds > 0 {
		input.MaximumDurationInSeconds = aws.Int32(req.MaximumSeconds)
	}
	if req.IdleSeconds > 0 {
		input.IdlePolicy = &lambdatypes.IdlePolicy{
			AutoResumeEnabled:        aws.Bool(true),
			MaxIdleDurationSeconds:   aws.Int32(req.IdleSeconds),
			SuspendedDurationSeconds: aws.Int32(req.SuspendedSeconds),
		}
	}
	out, err := c.client.RunMicrovm(ctx, input)
	if err != nil {
		return microVM{}, err
	}
	return microVMFromRun(out), nil
}

func (c *sdkControlPlane) Get(ctx context.Context, id string) (microVM, error) {
	out, err := c.client.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: aws.String(id)})
	if err != nil {
		return microVM{}, err
	}
	return microVM{
		ID:           aws.ToString(out.MicrovmId),
		Endpoint:     aws.ToString(out.Endpoint),
		ImageARN:     aws.ToString(out.ImageArn),
		ImageVersion: aws.ToString(out.ImageVersion),
		State:        string(out.State),
		StateReason:  aws.ToString(out.StateReason),
		StartedAt:    aws.ToTime(out.StartedAt),
	}, nil
}

func (c *sdkControlPlane) Probe(ctx context.Context, image, version string) error {
	input := &lambdamicrovms.ListMicrovmsInput{ImageIdentifier: aws.String(image), MaxResults: aws.Int32(1)}
	if version != "" {
		input.ImageVersion = aws.String(version)
	}
	_, err := c.client.ListMicrovms(ctx, input)
	return err
}

func (c *sdkControlPlane) Terminate(ctx context.Context, id string) error {
	_, err := c.client.TerminateMicrovm(ctx, &lambdamicrovms.TerminateMicrovmInput{MicrovmIdentifier: aws.String(id)})
	return err
}

func (c *sdkControlPlane) Suspend(ctx context.Context, id string) error {
	_, err := c.client.SuspendMicrovm(ctx, &lambdamicrovms.SuspendMicrovmInput{MicrovmIdentifier: aws.String(id)})
	return err
}

func (c *sdkControlPlane) Resume(ctx context.Context, id string) error {
	_, err := c.client.ResumeMicrovm(ctx, &lambdamicrovms.ResumeMicrovmInput{MicrovmIdentifier: aws.String(id)})
	return err
}

func (c *sdkControlPlane) AuthToken(ctx context.Context, id string) (string, error) {
	out, err := c.client.CreateMicrovmAuthToken(ctx, &lambdamicrovms.CreateMicrovmAuthTokenInput{
		MicrovmIdentifier:   aws.String(id),
		ExpirationInMinutes: aws.Int32(60),
		AllowedPorts: []lambdatypes.PortSpecification{
			&lambdatypes.PortSpecificationMemberPort{Value: runnerPort},
		},
	})
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(out.AuthToken["X-aws-proxy-auth"])
	if token == "" {
		return "", fmt.Errorf("AWS Lambda MicroVM auth response omitted X-aws-proxy-auth")
	}
	return token, nil
}

func microVMFromRun(out *lambdamicrovms.RunMicrovmOutput) microVM {
	return microVM{
		ID:           aws.ToString(out.MicrovmId),
		Endpoint:     aws.ToString(out.Endpoint),
		ImageARN:     aws.ToString(out.ImageArn),
		ImageVersion: aws.ToString(out.ImageVersion),
		State:        string(out.State),
		StateReason:  aws.ToString(out.StateReason),
		StartedAt:    aws.ToTime(out.StartedAt),
	}
}

func isNotFound(err error) bool {
	var apiErr smithy.APIError
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found") ||
		(err != nil && errors.As(err, &apiErr) && apiErr.ErrorCode() == "ResourceNotFoundException")
}

type runnerClient struct {
	control controlPlane
	http    *http.Client
	region  string
}

type runnerExecRequest struct {
	Command string            `json:"command"`
	Workdir string            `json:"workdir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type runnerEvent struct {
	Stream   string `json:"stream,omitempty"`
	Data     []byte `json:"data,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Error    string `json:"error,omitempty"`
}

func newRunnerClient(control controlPlane, source *http.Client, region string) *runnerClient {
	if source == nil {
		source = &http.Client{}
	}
	cloned := *source
	originalRedirect := source.CheckRedirect
	cloned.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 || !sameOrigin(req.URL, via[0].URL) {
			return fmt.Errorf("%s refused cross-origin redirect to %s", providerName, req.URL.Redacted())
		}
		if originalRedirect != nil {
			return originalRedirect(req, via)
		}
		return nil
	}
	return &runnerClient{control: control, http: &cloned, region: region}
}

func (c *runnerClient) Health(ctx context.Context, vm microVM) error {
	resp, err := c.do(ctx, vm, http.MethodGet, "/health", nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError("health", resp)
	}
	return nil
}

func (c *runnerClient) Upload(ctx context.Context, vm microVM, remotePath string, body io.Reader) error {
	requestPath := "/v1/files?path=" + url.QueryEscape(remotePath)
	resp, err := c.do(ctx, vm, http.MethodPut, requestPath, body, "application/gzip")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("upload", resp)
	}
	return nil
}

func (c *runnerClient) Exec(ctx context.Context, vm microVM, command, workdir string, env map[string]string, stdout, stderr io.Writer) (int, error) {
	payload, err := json.Marshal(runnerExecRequest{Command: command, Workdir: workdir, Env: env})
	if err != nil {
		return 1, err
	}
	resp, err := c.do(ctx, vm, http.MethodPost, "/v1/exec", bytes.NewReader(payload), "application/json")
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1, responseError("exec", resp)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	exitCode := -1
	for scanner.Scan() {
		var event runnerEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return 1, fmt.Errorf("decode %s runner event: %w", providerName, err)
		}
		switch event.Stream {
		case "stdout":
			_, _ = stdout.Write(event.Data)
		case "stderr":
			_, _ = stderr.Write(event.Data)
		}
		if event.Error != "" {
			return 1, fmt.Errorf("%s runner: %s", providerName, event.Error)
		}
		if event.ExitCode != nil {
			exitCode = *event.ExitCode
		}
	}
	if err := scanner.Err(); err != nil {
		return 1, fmt.Errorf("read %s runner stream: %w", providerName, err)
	}
	if exitCode < 0 {
		return 1, fmt.Errorf("%s runner stream ended without an exit code", providerName)
	}
	return exitCode, nil
}

func (c *runnerClient) do(ctx context.Context, vm microVM, method, requestPath string, body io.Reader, contentType string) (*http.Response, error) {
	base, err := endpointURL(vm.Endpoint, c.region)
	if err != nil {
		return nil, err
	}
	token, err := c.control.AuthToken(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	target := *base
	target.Path = requestPath
	target.RawQuery = ""
	if parsed, err := url.Parse(requestPath); err == nil {
		target.Path = parsed.Path
		target.RawQuery = parsed.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-aws-proxy-auth", token)
	req.Header.Set("X-aws-proxy-port", fmt.Sprint(runnerPort))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.http.Do(req)
}

func endpointURL(endpoint, region string) (*url.URL, error) {
	value := strings.TrimSpace(endpoint)
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	wantSuffix := ".lambda-microvm." + region + ".on.aws"
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" || !strings.HasSuffix(strings.ToLower(parsed.Hostname()), wantSuffix) || parsed.Path != "" || parsed.RawQuery != "" {
		return nil, fmt.Errorf("invalid AWS Lambda MicroVM endpoint for region %s", region)
	}
	return parsed, nil
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func responseError(action string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return fmt.Errorf("%s runner %s failed: status=%d body=%s", providerName, action, resp.StatusCode, strings.TrimSpace(string(body)))
}
