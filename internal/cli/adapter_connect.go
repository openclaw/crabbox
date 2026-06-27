package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"nhooyr.io/websocket"
)

const (
	adapterRelayMaxBodyBytes          = 64 << 10
	adapterRelayMaxMessageBytes       = adapterRelayMaxBodyBytes*6 + (16 << 10)
	adapterRelayRequestTimeout        = 9 * time.Second
	adapterRelayHandshakeTimeout      = 10 * time.Second
	adapterRelayDefaultConnectionTime = 2 * time.Minute
	adapterRelayMaxConnectionTime     = 24 * time.Hour
	adapterRelayConnectionOverhead    = 30 * time.Second
	adapterRelayWriteTimeout          = 5 * time.Second
	adapterRelayMinBackoff            = 250 * time.Millisecond
	adapterRelayMaxBackoff            = 5 * time.Second
	adapterRelayMaxConcurrentRequests = 64
	adapterRelayMaxConcurrentDeletes  = 8
)

type coordinatorAdapterTicket struct {
	Ticket    string `json:"ticket"`
	AdapterID string `json:"adapterID,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

type adapterRelayRequest struct {
	Type       string            `json:"type"`
	ID         string            `json:"id"`
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	DeadlineMS int64             `json:"deadlineMs"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       *string           `json:"body,omitempty"`
}

type adapterRelayResponse struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

type adapterRelay struct {
	localBaseURL   string
	loadLocalToken func() (string, error)
	client         *http.Client
	desktopTimeout time.Duration
	ws             *websocket.Conn
	writeMu        sync.Mutex
}

func (a App) adapterConnect(ctx context.Context, args []string) error {
	if err := adapterConnectHostSupported(); err != nil {
		return exit(2, "%v", err)
	}
	fs := newFlagSet("adapter connect", a.Stderr)
	id := fs.String("id", getenv("CRABBOX_ADAPTER_ID", ""), "coordinator adapter id")
	localSocket := fs.String("local-socket", getenv("CRABBOX_ADAPTER_LOCAL_SOCKET", ""), "current-user-owned local adapter Unix socket (required)")
	tokenFile := fs.String("token-file", getenv("CRABBOX_ADAPTER_TOKEN_FILE", ""), "file containing the local adapter bearer token (required)")
	connectionTimeout := fs.Duration("connection-timeout", controllerEnvDuration("CRABBOX_ADAPTER_CONNECTION_TIMEOUT", adapterRelayDefaultConnectionTime), "local adapter desktop connection setup duration")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return exit(2, "usage: crabbox adapter connect --id <adapter-id> --local-socket <path> --token-file <path> [--connection-timeout <duration>]")
	}
	adapterID := strings.TrimSpace(*id)
	if !validControllerWorkspaceID(adapterID) {
		return exit(2, "--id must be a lowercase DNS-style name with at most 63 characters")
	}
	if strings.TrimSpace(*tokenFile) == "" {
		return exit(2, "--token-file is required")
	}
	if *connectionTimeout <= 0 {
		return exit(2, "--connection-timeout must be greater than zero")
	}
	if *connectionTimeout > adapterRelayMaxConnectionTime {
		return exit(2, "--connection-timeout must not exceed %s", adapterRelayMaxConnectionTime)
	}
	desktopRequestTimeout := *connectionTimeout + adapterRelayConnectionOverhead
	if desktopRequestTimeout <= *connectionTimeout {
		return exit(2, "--connection-timeout is too large")
	}
	socketPath, err := normalizeAdapterUnixSocketPath(strings.TrimSpace(*localSocket))
	if err != nil {
		return exit(2, "--local-socket: %v", err)
	}
	loadLocalToken := func() (string, error) {
		return readAdapterToken(*tokenFile)
	}
	if _, err := loadLocalToken(); err != nil {
		return err
	}
	localClient, err := newAdapterLocalClient(socketPath, desktopRequestTimeout)
	if err != nil {
		return exit(2, "--local-socket: %v", err)
	}
	if _, err := configuredAdapterCoordinatorClient(); err != nil {
		return err
	}

	defer localClient.CloseIdleConnections()
	return runAdapterRelayLoop(ctx, a.Stderr, func(connectCtx context.Context) error {
		// Reload the normal Crabbox config before every ticket. A long-running
		// relay therefore picks up a refreshed coordinator session token without
		// persisting a second credential.
		coord, err := configuredAdapterCoordinatorClient()
		if err != nil {
			return err
		}
		if _, err := loadLocalToken(); err != nil {
			return err
		}
		return connectAdapterRelay(connectCtx, coord, adapterID, "http://adapter.local", socketPath, loadLocalToken, localClient, desktopRequestTimeout, a.Stdout)
	})
}

func configuredAdapterCoordinatorClient() (*CoordinatorClient, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	coord, configured, err := newCoordinatorClient(cfg)
	if err != nil {
		return nil, err
	}
	if !configured || coord == nil || strings.TrimSpace(coord.BaseURL) == "" {
		return nil, exit(2, "adapter connect requires a configured coordinator; run crabbox login --url <coordinator-url> first")
	}
	if !coord.hasConfiguredAuth() {
		return nil, exit(2, "adapter connect requires coordinator authentication; run crabbox login --url <coordinator-url> first")
	}
	return coord, nil
}

func runAdapterRelayLoop(ctx context.Context, log io.Writer, connect func(context.Context) error) error {
	for attempt := 0; ; attempt++ {
		err := connect(ctx)
		if ctx.Err() != nil {
			return nil
		}
		delay := adapterRelayBackoff(attempt)
		if log != nil {
			if err == nil {
				fmt.Fprintf(log, "adapter relay disconnected; reconnecting in %s\n", delay)
			} else {
				fmt.Fprintf(log, "adapter relay disconnected: %v; reconnecting in %s\n", err, delay)
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

func adapterRelayBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := adapterRelayMinBackoff
	for i := 0; i < attempt && delay < adapterRelayMaxBackoff; i++ {
		delay *= 2
		if delay >= adapterRelayMaxBackoff {
			return adapterRelayMaxBackoff
		}
	}
	return delay
}

func connectAdapterRelay(
	ctx context.Context,
	coord *CoordinatorClient,
	adapterID string,
	localBaseURL string,
	localSocket string,
	loadLocalToken func() (string, error),
	localClient *http.Client,
	desktopRequestTimeout time.Duration,
	status io.Writer,
) error {
	dialCtx, cancelDial := context.WithTimeout(ctx, adapterRelayHandshakeTimeout)
	defer cancelDial()
	coordinatorDesktopTimeout := desktopRequestTimeout + adapterRelayWriteTimeout
	if coordinatorDesktopTimeout <= desktopRequestTimeout {
		return errors.New("adapter relay desktop timeout is too large")
	}
	ticket, err := coord.CreateAdapterTicket(dialCtx, adapterID, coordinatorDesktopTimeout)
	if err != nil {
		return fmt.Errorf("create adapter ticket: %w", err)
	}
	if strings.TrimSpace(ticket.Ticket) == "" {
		return errors.New("coordinator returned an empty adapter ticket")
	}
	headers := bridgeTicketHeaders(coord, ticket.Ticket)
	dialOptions := &websocket.DialOptions{
		HTTPHeader: headers,
	}
	if coord.Client != nil {
		dialOptions.HTTPClient = coord.Client
	}
	ws, response, err := websocket.Dial(dialCtx, adapterRelayAgentURL(coord.BaseURL, adapterID), dialOptions)
	if err != nil {
		if response != nil && response.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
			_ = response.Body.Close()
		}
		return fmt.Errorf("connect adapter relay: %w", err)
	}
	ws.SetReadLimit(adapterRelayMaxMessageBytes)
	relay := &adapterRelay{
		localBaseURL:   localBaseURL,
		loadLocalToken: loadLocalToken,
		client:         localClient,
		desktopTimeout: desktopRequestTimeout,
		ws:             ws,
	}
	defer ws.Close(websocket.StatusNormalClosure, "adapter relay stopped")
	if status != nil {
		fmt.Fprintf(status, "adapter relay connected id=%s coordinator=%s local_socket=%s\n", adapterID, coord.BaseURL, localSocket)
	}
	return relay.serve(ctx)
}

func (c *CoordinatorClient) CreateAdapterTicket(ctx context.Context, adapterID string, desktopTimeout time.Duration) (coordinatorAdapterTicket, error) {
	var result coordinatorAdapterTicket
	err := c.do(ctx, http.MethodPost, "/v1/adapters/"+url.PathEscape(adapterID)+"/ticket", map[string]any{
		"desktopTimeoutMs": desktopTimeout.Milliseconds(),
	}, &result)
	return result, err
}

func adapterRelayAgentURL(baseURL, adapterID string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return baseURL
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/adapters/" + url.PathEscape(adapterID) + "/agent"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func (r *adapterRelay) serve(ctx context.Context) error {
	relayCtx, cancel := context.WithCancelCause(ctx)
	requests := make(chan struct{}, adapterRelayMaxConcurrentRequests)
	deletes := make(chan struct{}, adapterRelayMaxConcurrentDeletes)
	var workers sync.WaitGroup
	defer func() {
		cancel(context.Canceled)
		workers.Wait()
	}()
	for {
		messageType, data, err := r.ws.Read(relayCtx)
		if err != nil {
			if cause := context.Cause(relayCtx); cause != nil && !errors.Is(cause, context.Canceled) {
				return cause
			}
			return err
		}
		if messageType != websocket.MessageText {
			return errors.New("adapter relay accepts text messages only")
		}
		request, err := decodeAdapterRelayRequest(data)
		if err != nil {
			return fmt.Errorf("decode adapter relay request: %w", err)
		}
		limit := requests
		if request.Method == http.MethodDelete {
			// Keep cancellation available even while slow desktop setup requests
			// occupy every ordinary relay slot.
			limit = deletes
		}
		select {
		case limit <- struct{}{}:
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { <-limit }()
				response := r.handle(relayCtx, request)
				if err := r.writeResponse(relayCtx, response); err != nil {
					cancel(err)
				}
			}()
		default:
			response := adapterRelayErrorResponse(request.ID, http.StatusTooManyRequests, "adapter_busy", "adapter relay concurrency limit reached")
			if err := r.writeResponse(relayCtx, response); err != nil {
				cancel(err)
				return err
			}
		}
	}
}

func (r *adapterRelay) writeResponse(ctx context.Context, response adapterRelayResponse) error {
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, adapterRelayWriteTimeout)
	defer cancel()
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	return r.ws.Write(writeCtx, websocket.MessageText, data)
}

func decodeAdapterRelayRequest(data []byte) (adapterRelayRequest, error) {
	var request adapterRelayRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return request, errors.New("request must contain one JSON object")
		}
		return request, err
	}
	return request, nil
}

func (r *adapterRelay) handle(ctx context.Context, request adapterRelayRequest) adapterRelayResponse {
	if err := validateAdapterRelayRequest(request); err != nil {
		return adapterRelayErrorResponse(request.ID, http.StatusBadRequest, "invalid_request", err.Error())
	}
	deadline := time.UnixMilli(request.DeadlineMS)
	if !deadline.After(time.Now()) {
		return adapterRelayErrorResponse(request.ID, http.StatusGatewayTimeout, "adapter_timeout", "adapter relay request expired before local dispatch")
	}
	body := []byte(nil)
	if request.Body != nil {
		body = []byte(*request.Body)
	}
	deadlineCtx, cancelDeadline := context.WithDeadline(ctx, deadline)
	defer cancelDeadline()
	requestCtx, cancelTimeout := context.WithTimeout(deadlineCtx, adapterRelayTimeoutForRequest(request, r.desktopTimeout))
	defer cancelTimeout()
	localPath, ok := adapterRelayCanonicalPath(request.Method, request.Path)
	if !ok {
		return adapterRelayErrorResponse(request.ID, http.StatusBadRequest, "invalid_request", "method and path are outside the crabfleet/v1 adapter surface")
	}
	localRequest, err := http.NewRequestWithContext(requestCtx, request.Method, r.localBaseURL+localPath, bytes.NewReader(body))
	if err != nil {
		return adapterRelayErrorResponse(request.ID, http.StatusBadRequest, "invalid_request", "could not construct local adapter request")
	}
	for key, value := range request.Headers {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "accept":
			localRequest.Header.Set("Accept", value)
		case "content-type":
			localRequest.Header.Set("Content-Type", value)
		case "idempotency-key":
			localRequest.Header.Set("Idempotency-Key", value)
		}
	}
	localToken, err := r.loadLocalToken()
	if err != nil {
		return adapterRelayErrorResponse(request.ID, http.StatusBadGateway, "adapter_auth_unavailable", "local adapter token could not be loaded")
	}
	localRequest.Header.Set("Authorization", "Bearer "+localToken)
	localRequest.Header.Set("Accept", "application/json")
	if request.Method == http.MethodPost && request.Path == "/v1/workspaces" && localRequest.Header.Get("Content-Type") == "" {
		localRequest.Header.Set("Content-Type", "application/json")
	}
	if requestCtx.Err() != nil {
		return adapterRelayErrorResponse(request.ID, http.StatusGatewayTimeout, "adapter_timeout", "adapter relay request expired before local dispatch")
	}

	localResponse, err := r.client.Do(localRequest)
	if err != nil {
		if requestCtx.Err() != nil && ctx.Err() == nil {
			return adapterRelayErrorResponse(request.ID, http.StatusGatewayTimeout, "adapter_timeout", "local adapter request timed out")
		}
		return adapterRelayErrorResponse(request.ID, http.StatusBadGateway, "adapter_unavailable", "local adapter request failed")
	}
	defer localResponse.Body.Close()
	responseBody, err := readAdapterRelayResponseBody(localResponse.Body)
	if err != nil {
		return adapterRelayErrorResponse(request.ID, http.StatusBadGateway, "invalid_adapter_response", err.Error())
	}
	response := adapterRelayResponse{
		Type:   "response",
		ID:     request.ID,
		Status: localResponse.StatusCode,
		Body:   responseBody,
	}
	if contentType := strings.TrimSpace(localResponse.Header.Get("Content-Type")); contentType != "" {
		response.Headers = map[string]string{"content-type": contentType}
	}
	return response
}

func adapterRelayTimeoutForRequest(request adapterRelayRequest, desktopTimeout time.Duration) time.Duration {
	if request.Method == http.MethodPost && (strings.HasSuffix(request.Path, "/connections/desktop") || strings.HasSuffix(request.Path, "/connections/native-vnc")) {
		if desktopTimeout <= 0 {
			desktopTimeout = adapterRelayDefaultConnectionTime + adapterRelayConnectionOverhead
		}
		return desktopTimeout
	}
	return adapterRelayRequestTimeout
}

func validateAdapterRelayRequest(request adapterRelayRequest) error {
	if request.Type != "request" {
		return errors.New("type must be request")
	}
	if !validAdapterRelayRequestID(request.ID) {
		return errors.New("id must be 1 to 128 printable characters")
	}
	if request.DeadlineMS <= 0 {
		return errors.New("deadlineMs must be a positive Unix millisecond timestamp")
	}
	if request.Method != strings.ToUpper(request.Method) || !adapterRelayRouteAllowed(request.Method, request.Path) {
		return errors.New("method and path are outside the crabfleet/v1 adapter surface")
	}
	if request.Body != nil && len([]byte(*request.Body)) > adapterRelayMaxBodyBytes {
		return errors.New("body exceeds 64 KiB")
	}
	if request.Body != nil && *request.Body != "" && request.Method != http.MethodPost {
		return errors.New("request body is not allowed for this method")
	}
	if request.Body != nil && *request.Body != "" && request.Path != "/v1/workspaces" {
		return errors.New("request body is allowed only for workspace creation")
	}
	if err := validateAdapterRelayHeaders(request.Headers); err != nil {
		return err
	}
	return nil
}

func validAdapterRelayRequestID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func adapterRelayRouteAllowed(method, requestPath string) bool {
	_, ok := adapterRelayCanonicalPath(method, requestPath)
	return ok
}

func adapterRelayCanonicalPath(method, requestPath string) (string, bool) {
	if requestPath == "/v1/workspaces" {
		return requestPath, method == http.MethodPost
	}
	const prefix = "/v1/workspaces/"
	if !strings.HasPrefix(requestPath, prefix) || strings.ContainsAny(requestPath, "?#%") {
		return "", false
	}
	rest := strings.TrimPrefix(requestPath, prefix)
	if method == http.MethodPost && (strings.HasSuffix(rest, "/connections/desktop") || strings.HasSuffix(rest, "/connections/native-vnc")) {
		suffix := "/connections/desktop"
		if strings.HasSuffix(rest, "/connections/native-vnc") {
			suffix = "/connections/native-vnc"
		}
		workspaceID := strings.TrimSuffix(rest, suffix)
		if !validControllerWorkspaceID(workspaceID) {
			return "", false
		}
		return prefix + url.PathEscape(workspaceID) + suffix, true
	}
	if (method != http.MethodGet && method != http.MethodDelete) || !validControllerWorkspaceID(rest) {
		return "", false
	}
	return prefix + url.PathEscape(rest), true
}

func validateAdapterRelayHeaders(headers map[string]string) error {
	if len(headers) > 16 {
		return errors.New("too many request headers")
	}
	total := 0
	for key, value := range headers {
		if len(key) == 0 || len(key) > 64 || len(value) > 4096 || strings.ContainsAny(key, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return errors.New("request header is invalid")
		}
		total += len(key) + len(value)
	}
	if total > 8<<10 {
		return errors.New("request headers exceed 8 KiB")
	}
	return nil
}

func readAdapterRelayResponseBody(body io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(body, adapterRelayMaxBodyBytes+1))
	if err != nil {
		return "", errors.New("could not read local adapter response")
	}
	if len(data) > adapterRelayMaxBodyBytes {
		return "", errors.New("local adapter response exceeds 64 KiB")
	}
	if !utf8.Valid(data) {
		return "", errors.New("local adapter response is not UTF-8")
	}
	return string(data), nil
}

func adapterRelayErrorResponse(id string, status int, code, message string) adapterRelayResponse {
	if !validAdapterRelayRequestID(id) {
		id = ""
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
	return adapterRelayResponse{
		Type:    "response",
		ID:      id,
		Status:  status,
		Headers: map[string]string{"content-type": "application/json; charset=utf-8"},
		Body:    string(body),
	}
}
