package tencentcloud

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	cvmService = "cvm"
	tagService = "tag"
	stsService = "sts"

	cvmVersion = "2017-03-12"
	tagVersion = "2018-08-13"
	stsVersion = "2018-08-13"
)

type tencentCloudAPI interface {
	AccountID(context.Context) (string, error)
	ListInstances(context.Context) ([]instance, error)
	GetInstance(context.Context, string) (instance, error)
	RunInstance(context.Context, runInstanceRequest) (string, error)
	TerminateInstance(context.Context, string) error
	ReplaceInstanceTags(context.Context, string, []tag, []tag) error
}

type client struct {
	secretID     string
	secretKey    string
	token        string
	httpClient   *http.Client
	region       string
	cvmEndpoint  string
	tagEndpoint  string
	stsEndpoint  string
	accountID    string
	accountReady bool
}

type apiError struct {
	Action    string
	Code      string
	Message   string
	RequestID string
	Status    int
	Body      string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("tencentcloud %s: %s: %s request=%s", e.Action, e.Code, e.Message, e.RequestID)
	}
	return fmt.Sprintf("tencentcloud %s: http %d: %s", e.Action, e.Status, e.Body)
}

func newClient(cfg core.Config, rt core.Runtime) (*client, error) {
	secretID := strings.TrimSpace(os.Getenv("TENCENTCLOUD_SECRET_ID"))
	secretKey := strings.TrimSpace(os.Getenv("TENCENTCLOUD_SECRET_KEY"))
	if secretID == "" || secretKey == "" {
		return nil, core.Exit(3, "TENCENTCLOUD_SECRET_ID and TENCENTCLOUD_SECRET_KEY are required")
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &client{
		secretID:    secretID,
		secretKey:   secretKey,
		token:       strings.TrimSpace(os.Getenv("TENCENTCLOUD_TOKEN")),
		httpClient:  httpClient,
		region:      regionForConfig(cfg),
		cvmEndpoint: cvmEndpointForConfig(cfg),
		tagEndpoint: tagEndpointForConfig(cfg),
		stsEndpoint: stsEndpointForConfig(cfg),
	}, nil
}

func (c *client) AccountID(ctx context.Context) (string, error) {
	if c.accountReady {
		return c.accountID, nil
	}
	if value := strings.TrimSpace(os.Getenv("TENCENTCLOUD_ACCOUNT_ID")); value != "" {
		c.accountID = value
		c.accountReady = true
		return c.accountID, nil
	}
	var out getCallerIdentityResponse
	if err := c.do(ctx, stsService, c.stsEndpoint, "GetCallerIdentity", stsVersion, c.region, map[string]any{}, &out); err != nil {
		return "", err
	}
	c.accountID = firstNonBlank(out.AccountID, out.UIN)
	if c.accountID == "" {
		return "", core.Exit(3, "tencentcloud GetCallerIdentity response did not include AccountId or Uin")
	}
	c.accountReady = true
	return c.accountID, nil
}

func (c *client) ListInstances(ctx context.Context) ([]instance, error) {
	var out []instance
	for offset := int64(0); ; offset += 100 {
		var res describeInstancesResponse
		err := c.do(ctx, cvmService, c.cvmEndpoint, "DescribeInstances", cvmVersion, c.region, map[string]any{
			"Offset": offset,
			"Limit":  int64(100),
		}, &res)
		if err != nil {
			return nil, err
		}
		out = append(out, res.InstanceSet...)
		if int64(len(out)) >= res.TotalCount || len(res.InstanceSet) == 0 {
			return out, nil
		}
	}
}

func (c *client) GetInstance(ctx context.Context, id string) (instance, error) {
	var res describeInstancesResponse
	err := c.do(ctx, cvmService, c.cvmEndpoint, "DescribeInstances", cvmVersion, c.region, map[string]any{
		"InstanceIds": []string{id},
	}, &res)
	if err != nil {
		return instance{}, err
	}
	if len(res.InstanceSet) == 0 {
		return instance{}, &apiError{Action: "DescribeInstances", Code: "ResourceNotFound.Instance", Message: "instance not found"}
	}
	return res.InstanceSet[0], nil
}

func (c *client) RunInstance(ctx context.Context, req runInstanceRequest) (string, error) {
	var res runInstancesResponse
	if err := c.do(ctx, cvmService, c.cvmEndpoint, "RunInstances", cvmVersion, c.region, req, &res); err != nil {
		return "", err
	}
	if len(res.InstanceIDSet) == 0 {
		return "", core.Exit(3, "tencentcloud RunInstances response did not include InstanceIdSet")
	}
	return res.InstanceIDSet[0], nil
}

func (c *client) TerminateInstance(ctx context.Context, id string) error {
	return c.do(ctx, cvmService, c.cvmEndpoint, "TerminateInstances", cvmVersion, c.region, map[string]any{
		"InstanceIds": []string{id},
	}, nil)
}

func (c *client) ReplaceInstanceTags(ctx context.Context, instanceID string, currentTags, desiredTags []tag) error {
	accountID, err := c.AccountID(ctx)
	if err != nil {
		return err
	}
	replaceTags := tagUpdateSet(desiredTags)
	deleteTags := tagDeleteSet(currentTags, desiredTags)
	if len(replaceTags) == 0 && len(deleteTags) == 0 {
		return nil
	}
	body := map[string]any{
		"Resource": resourceName(c.region, accountID, instanceID),
	}
	if len(replaceTags) > 0 {
		body["ReplaceTags"] = replaceTags
	}
	if len(deleteTags) > 0 {
		body["DeleteTags"] = deleteTags
	}
	return c.do(ctx, tagService, c.tagEndpoint, "ModifyResourceTags", tagVersion, c.region, body, nil)
}

func (c *client) do(ctx context.Context, service, endpoint, action, version, region string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("invalid Tencent Cloud endpoint %q", endpoint)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	signTencentCloudRequest(req, signInput{
		SecretID:  c.secretID,
		SecretKey: c.secretKey,
		Token:     c.token,
		Service:   service,
		Action:    action,
		Version:   version,
		Region:    region,
		Timestamp: now.Unix(),
		Payload:   payload,
	})
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if len(data) > 800 {
			data = data[:800]
		}
		body := c.redact(strings.TrimSpace(string(data)))
		if readErr != nil {
			if body != "" {
				body += "; "
			}
			body += "response body read failed: " + readErr.Error()
		}
		return &apiError{Action: action, Status: resp.StatusCode, Body: body}
	}
	if readErr != nil {
		return fmt.Errorf("tencentcloud %s response body: %w", action, readErr)
	}
	if len(data) == 0 {
		return nil
	}
	var envelope struct {
		Response json.RawMessage `json:"Response"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("tencentcloud %s decode envelope: %w", action, err)
	}
	if len(envelope.Response) == 0 {
		return fmt.Errorf("tencentcloud %s response missing Response", action)
	}
	var probe struct {
		Error *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
		RequestID string `json:"RequestId"`
	}
	if err := json.Unmarshal(envelope.Response, &probe); err == nil && probe.Error != nil {
		return &apiError{Action: action, Code: probe.Error.Code, Message: c.redact(probe.Error.Message), RequestID: probe.RequestID}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Response, out); err != nil {
		return fmt.Errorf("tencentcloud %s decode response: %w", action, err)
	}
	return nil
}

func (c *client) redact(value string) string {
	for _, secret := range []string{c.secretID, c.secretKey, c.token} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "<redacted>")
		}
	}
	return value
}

type signInput struct {
	SecretID  string
	SecretKey string
	Token     string
	Service   string
	Action    string
	Version   string
	Region    string
	Timestamp int64
	Payload   []byte
}

func signTencentCloudRequest(req *http.Request, in signInput) {
	const algorithm = "TC3-HMAC-SHA256"
	date := time.Unix(in.Timestamp, 0).UTC().Format("2006-01-02")
	hashedPayload := sha256Hex(in.Payload)
	canonicalHeaders := "content-type:application/json; charset=utf-8\nhost:" + req.URL.Host + "\nx-tc-action:" + strings.ToLower(in.Action) + "\n"
	signedHeaders := "content-type;host;x-tc-action"
	canonicalRequest := strings.Join([]string{
		http.MethodPost,
		"/",
		"",
		canonicalHeaders,
		signedHeaders,
		hashedPayload,
	}, "\n")
	credentialScope := date + "/" + in.Service + "/tc3_request"
	stringToSign := strings.Join([]string{
		algorithm,
		fmt.Sprint(in.Timestamp),
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	secretDate := hmacSHA256([]byte("TC3"+in.SecretKey), date)
	secretService := hmacSHA256(secretDate, in.Service)
	secretSigning := hmacSHA256(secretService, "tc3_request")
	signature := hex.EncodeToString(hmacSHA256(secretSigning, stringToSign))
	authorization := fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s", algorithm, in.SecretID, credentialScope, signedHeaders, signature)

	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-TC-Action", in.Action)
	req.Header.Set("X-TC-Version", in.Version)
	req.Header.Set("X-TC-Timestamp", fmt.Sprint(in.Timestamp))
	if in.Region != "" {
		req.Header.Set("X-TC-Region", in.Region)
	}
	if in.Token != "" {
		req.Header.Set("X-TC-Token", in.Token)
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(value))
	return h.Sum(nil)
}

func resourceName(region, accountID, instanceID string) string {
	return fmt.Sprintf("qcs::cvm:%s:uin/%s:instance/%s", region, accountID, instanceID)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func isNotFound(err error) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	return strings.Contains(strings.ToLower(apiErr.Code), "notfound") || strings.Contains(strings.ToLower(apiErr.Code), "not found")
}
