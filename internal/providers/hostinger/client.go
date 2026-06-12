package hostinger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type hostingerAPI interface {
	ListCatalog(ctx context.Context) ([]hostingerCatalogItem, error)
	ListPaymentMethods(ctx context.Context) ([]hostingerPaymentMethod, error)
	ListDataCenters(ctx context.Context) ([]hostingerDataCenter, error)
	ListTemplates(ctx context.Context) ([]hostingerTemplate, error)
	ListVMs(ctx context.Context) ([]hostingerVM, error)
	GetVM(ctx context.Context, id string) (hostingerVM, error)
	PurchaseVM(ctx context.Context, input hostingerPurchaseInput) (hostingerVM, error)
	SetupVM(ctx context.Context, id string, input hostingerSetupInput) (hostingerVM, error)
	StartVM(ctx context.Context, id string) error
	StopVM(ctx context.Context, id string) error
}

const hostingerMaxResponseBytes = 16 << 20

type hostingerClient struct {
	token      string
	apiURL     string
	httpClient *http.Client
}

type hostingerAPIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *hostingerAPIError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

type hostingerDataCenter struct {
	ID       any    `json:"id"`
	Name     string `json:"name"`
	Location string `json:"location"`
}

type hostingerTemplate struct {
	ID   any    `json:"id"`
	Name string `json:"name"`
	OS   string `json:"os"`
}

type hostingerCatalogItem struct {
	ID       string                  `json:"id"`
	Name     string                  `json:"name"`
	Category string                  `json:"category"`
	Prices   []hostingerCatalogPrice `json:"prices"`
}

type hostingerCatalogPrice struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Currency         string `json:"currency"`
	Price            int64  `json:"price"`
	FirstPeriodPrice int64  `json:"first_period_price"`
	Period           int64  `json:"period"`
	PeriodUnit       string `json:"period_unit"`
}

type hostingerPaymentMethod struct {
	ID            any    `json:"id"`
	Name          string `json:"name"`
	PaymentMethod string `json:"payment_method"`
	IsDefault     bool   `json:"is_default"`
	IsExpired     bool   `json:"is_expired"`
	IsSuspended   bool   `json:"is_suspended"`
}

type hostingerVM struct {
	ID         any                  `json:"id"`
	Name       string               `json:"name"`
	Hostname   string               `json:"hostname"`
	State      string               `json:"state"`
	Status     string               `json:"status"`
	IPv4       hostingerIPAddresses `json:"ipv4"`
	IP         string               `json:"ip"`
	IPV4       []string             `json:"ipv4_addresses"`
	ExternalIP string               `json:"external_ip"`
}

type hostingerIPAddresses []string

func (ips *hostingerIPAddresses) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		if single != "" {
			*ips = []string{single}
		}
		return nil
	}
	var values []string
	if err := json.Unmarshal(data, &values); err == nil {
		*ips = values
		return nil
	}
	var objects []struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(data, &objects); err != nil {
		return err
	}
	out := make([]string, 0, len(objects))
	for _, object := range objects {
		if strings.TrimSpace(object.Address) != "" {
			out = append(out, strings.TrimSpace(object.Address))
		}
	}
	*ips = out
	return nil
}

func (ips hostingerIPAddresses) First() string {
	if len(ips) == 0 {
		return ""
	}
	return strings.TrimSpace(ips[0])
}

type hostingerPurchaseInput struct {
	ItemID          string              `json:"item_id,omitempty"`
	PaymentMethodID int64               `json:"payment_method_id,omitempty"`
	Setup           hostingerSetupInput `json:"setup,omitempty"`
}

type hostingerSetupInput struct {
	TemplateID    int64                    `json:"template_id,omitempty"`
	DataCenterID  int64                    `json:"data_center_id,omitempty"`
	Hostname      string                   `json:"hostname,omitempty"`
	EnableBackups bool                     `json:"enable_backups"`
	PublicKey     *hostingerSetupPublicKey `json:"public_key,omitempty"`
}

type hostingerSetupPublicKey struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

func newClient(cfg Config, rt Runtime) (hostingerAPI, error) {
	token := strings.TrimSpace(cfg.Hostinger.APIToken)
	if token == "" {
		return nil, exit(2, "provider=%s requires HOSTINGER_API_TOKEN (CRABBOX_HOSTINGER_API_TOKEN also accepted)", providerName)
	}
	apiURL := strings.TrimRight(strings.TrimSpace(blank(cfg.Hostinger.APIURL, "https://developers.hostinger.com")), "/")
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, exit(2, "%s url %q is invalid", providerName, apiURL)
	}
	if parsed.Scheme != "https" && !isLoopbackHTTPURL(parsed) {
		return nil, exit(2, "%s url %q must use https unless it targets localhost", providerName, apiURL)
	}
	httpClient := rt.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	httpClient = secureHostingerAPIClient(httpClient, parsed)
	return &hostingerClient{token: token, apiURL: apiURL, httpClient: httpClient}, nil
}

func isLoopbackHTTPURL(u *url.URL) bool {
	if u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func secureHostingerAPIClient(source *http.Client, trusted *url.URL) *http.Client {
	client := *source
	originalCheckRedirect := source.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameHostingerOrigin(trusted, req.URL) {
			return fmt.Errorf("hostinger refused cross-origin or insecure redirect to %s", req.URL.Redacted())
		}
		if originalCheckRedirect != nil {
			return originalCheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &client
}

func sameHostingerOrigin(a, b *url.URL) bool {
	return a != nil && b != nil &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveHostingerPort(a) == effectiveHostingerPort(b)
}

func effectiveHostingerPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(u.Scheme, "http") {
		return "80"
	}
	return ""
}

func (c *hostingerClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, hostingerMaxResponseBytes+1))
	if readErr != nil {
		return readErr
	}
	if len(data) > hostingerMaxResponseBytes {
		return fmt.Errorf("hostinger response exceeds %d bytes", hostingerMaxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &hostingerAPIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: redactToken(c.token, strings.TrimSpace(string(data)))}
	}
	if out != nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode hostinger data: %w", err)
		}
	}
	return nil
}

func collection[T any](data []T) []T {
	if data == nil {
		return []T{}
	}
	return data
}

func (c *hostingerClient) ListCatalog(ctx context.Context) ([]hostingerCatalogItem, error) {
	var out []hostingerCatalogItem
	err := c.do(ctx, http.MethodGet, "/api/billing/v1/catalog?category=VPS", nil, &out)
	return collection(out), err
}

func (c *hostingerClient) ListPaymentMethods(ctx context.Context) ([]hostingerPaymentMethod, error) {
	var out []hostingerPaymentMethod
	err := c.do(ctx, http.MethodGet, "/api/billing/v1/payment-methods", nil, &out)
	return collection(out), err
}

func (c *hostingerClient) ListDataCenters(ctx context.Context) ([]hostingerDataCenter, error) {
	var out []hostingerDataCenter
	err := c.do(ctx, http.MethodGet, "/api/vps/v1/data-centers", nil, &out)
	return collection(out), err
}

func (c *hostingerClient) ListTemplates(ctx context.Context) ([]hostingerTemplate, error) {
	var out []hostingerTemplate
	err := c.do(ctx, http.MethodGet, "/api/vps/v1/templates", nil, &out)
	return collection(out), err
}

func (c *hostingerClient) ListVMs(ctx context.Context) ([]hostingerVM, error) {
	var out []hostingerVM
	err := c.do(ctx, http.MethodGet, "/api/vps/v1/virtual-machines", nil, &out)
	return collection(out), err
}

func (c *hostingerClient) GetVM(ctx context.Context, id string) (hostingerVM, error) {
	var out hostingerVM
	err := c.do(ctx, http.MethodGet, "/api/vps/v1/virtual-machines/"+url.PathEscape(id), nil, &out)
	return out, err
}

func (c *hostingerClient) PurchaseVM(ctx context.Context, input hostingerPurchaseInput) (hostingerVM, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodPost, "/api/vps/v1/virtual-machines", input, &raw); err != nil {
		return hostingerVM{}, err
	}
	var wrapped struct {
		VirtualMachine hostingerVM `json:"virtual_machine"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && (wrapped.VirtualMachine.IDString() != "" || wrapped.VirtualMachine.Host() != "") {
		return wrapped.VirtualMachine, nil
	}
	var out hostingerVM
	if err := json.Unmarshal(raw, &out); err != nil {
		return hostingerVM{}, fmt.Errorf("decode hostinger virtual machine purchase: %w", err)
	}
	return out, nil
}

func (c *hostingerClient) SetupVM(ctx context.Context, id string, input hostingerSetupInput) (hostingerVM, error) {
	var out hostingerVM
	err := c.do(ctx, http.MethodPost, "/api/vps/v1/virtual-machines/"+url.PathEscape(id)+"/setup", input, &out)
	return out, err
}

func (c *hostingerClient) StartVM(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/vps/v1/virtual-machines/"+url.PathEscape(id)+"/start", nil, nil)
}

func (c *hostingerClient) StopVM(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/vps/v1/virtual-machines/"+url.PathEscape(id)+"/stop", nil, nil)
}

func hostingerIDString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case json.Number:
		return v.String()
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func hostingerIntegerID(name, value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, exit(2, "provider=%s requires numeric hostinger %s, got %q", providerName, name, value)
	}
	return parsed, nil
}

func redactToken(token, value string) string {
	token = strings.TrimSpace(token)
	if token == "" || value == "" {
		return value
	}
	return strings.ReplaceAll(value, token, "[redacted]")
}
