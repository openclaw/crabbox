package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestAdapterRelayForwardsOnlyAllowedLocalRequestData(t *testing.T) {
	body := " {\n  \"id\": \"fleet-a-is-101\"\n}\n"
	var gotMethod, gotPath, gotBody, gotAuthorization, gotIdempotency, gotCookie, gotAccess string
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.RequestURI()
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		gotAuthorization = r.Header.Get("Authorization")
		gotIdempotency = r.Header.Get("Idempotency-Key")
		gotCookie = r.Header.Get("Cookie")
		gotAccess = r.Header.Get("CF-Access-Client-Secret")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "{\"id\":\"fleet-a-is-101\",\"status\":\"provisioning\"}\n")
	}))
	defer local.Close()

	relay := adapterRelay{
		localBaseURL:   local.URL,
		loadLocalToken: func() (string, error) { return "local-secret", nil },
		client:         local.Client(),
	}
	response := relay.handle(context.Background(), adapterRelayRequest{
		Type:   "request",
		ID:     "req-1",
		Method: http.MethodPost,
		Path:   "/v1/workspaces",
		Headers: map[string]string{
			"Authorization":           "Bearer remote-secret",
			"Cookie":                  "session=remote-secret",
			"CF-Access-Client-Secret": "remote-access-secret",
			"Content-Type":            "application/json",
			"Idempotency-Key":         "fleet-a-is-101",
		},
		Body: &body,
	})

	if gotMethod != http.MethodPost || gotPath != "/v1/workspaces" || gotBody != body {
		t.Fatalf("local request method=%q path=%q body=%q", gotMethod, gotPath, gotBody)
	}
	if gotAuthorization != "Bearer local-secret" || gotIdempotency != "fleet-a-is-101" {
		t.Fatalf("local auth=%q idempotency=%q", gotAuthorization, gotIdempotency)
	}
	if gotCookie != "" || gotAccess != "" {
		t.Fatalf("remote credentials reached local adapter cookie=%q access=%q", gotCookie, gotAccess)
	}
	if response.Type != "response" || response.ID != "req-1" || response.Status != http.StatusAccepted {
		t.Fatalf("response=%#v", response)
	}
	if response.Headers["content-type"] != "application/json; charset=utf-8" || response.Body != "{\"id\":\"fleet-a-is-101\",\"status\":\"provisioning\"}\n" {
		t.Fatalf("response headers=%v body=%q", response.Headers, response.Body)
	}
}

func TestAdapterRelayRouteAllowlist(t *testing.T) {
	for _, test := range []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodPost, "/v1/workspaces", true},
		{http.MethodGet, "/v1/workspaces/fleet-a-is-101", true},
		{http.MethodDelete, "/v1/workspaces/fleet-a-is-101", true},
		{http.MethodPost, "/v1/workspaces/fleet-a-is-101/connections/desktop", true},
		{http.MethodGet, "/healthz", false},
		{http.MethodPut, "/v1/workspaces/fleet-a-is-101", false},
		{http.MethodPost, "/v1/workspaces/fleet-a-is-101/reboot", false},
		{http.MethodGet, "/v1/workspaces/fleet-a-is-101?full=1", false},
		{http.MethodGet, "/v1/workspaces/fleet-a-is-101%2Fother", false},
		{http.MethodDelete, "/v1/workspaces/../other", false},
	} {
		if got := adapterRelayRouteAllowed(test.method, test.path); got != test.want {
			t.Errorf("route %s %s allowed=%v want=%v", test.method, test.path, got, test.want)
		}
		if canonical, ok := adapterRelayCanonicalPath(test.method, test.path); ok && canonical != test.path {
			t.Errorf("route %s %s canonical=%q", test.method, test.path, canonical)
		}
	}
}

func TestAdapterRelayRejectsBodiesAndResponsesOver64KiB(t *testing.T) {
	var calls atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, strings.Repeat("x", adapterRelayMaxBodyBytes+1))
	}))
	defer local.Close()
	relay := adapterRelay{
		localBaseURL:   local.URL,
		loadLocalToken: func() (string, error) { return "local-secret", nil },
		client:         local.Client(),
	}

	tooLarge := strings.Repeat("x", adapterRelayMaxBodyBytes+1)
	response := relay.handle(context.Background(), adapterRelayRequest{
		Type: "request", ID: "req-large", Method: http.MethodPost, Path: "/v1/workspaces", Body: &tooLarge,
	})
	if response.Status != http.StatusBadRequest || calls.Load() != 0 || !strings.Contains(response.Body, "body exceeds 64 KiB") {
		t.Fatalf("oversized request response=%#v calls=%d", response, calls.Load())
	}

	response = relay.handle(context.Background(), adapterRelayRequest{
		Type: "request", ID: "req-response-large", Method: http.MethodGet, Path: "/v1/workspaces/fleet-a-is-101",
	})
	if response.Status != http.StatusBadGateway || calls.Load() != 1 || !strings.Contains(response.Body, "response exceeds 64 KiB") {
		t.Fatalf("oversized local response=%#v calls=%d", response, calls.Load())
	}
}

func TestAdapterRelayRejectsRemoteBodyOnDelete(t *testing.T) {
	body := `{}`
	err := validateAdapterRelayRequest(adapterRelayRequest{
		Type: "request", ID: "req-delete", Method: http.MethodDelete, Path: "/v1/workspaces/fleet-a-is-101", Body: &body,
	})
	if err == nil || !strings.Contains(err.Error(), "body is not allowed") {
		t.Fatalf("delete body validation error=%v", err)
	}
	empty := ""
	if err := validateAdapterRelayRequest(adapterRelayRequest{
		Type: "request", ID: "req-delete-empty", Method: http.MethodDelete, Path: "/v1/workspaces/fleet-a-is-101", Body: &empty,
	}); err != nil {
		t.Fatalf("explicit empty delete body rejected: %v", err)
	}
}

func TestAdapterRelayReloadsLocalTokenForEveryRequest(t *testing.T) {
	token := "first-token"
	var authorizations []string
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{}`)
	}))
	defer local.Close()
	relay := adapterRelay{
		localBaseURL:   local.URL,
		loadLocalToken: func() (string, error) { return token, nil },
		client:         local.Client(),
	}
	request := adapterRelayRequest{Type: "request", ID: "request-1", Method: http.MethodGet, Path: "/v1/workspaces/fleet-a-is-101"}
	if response := relay.handle(context.Background(), request); response.Status != http.StatusOK {
		t.Fatalf("first response=%#v", response)
	}
	token = "rotated-token"
	request.ID = "request-2"
	if response := relay.handle(context.Background(), request); response.Status != http.StatusOK {
		t.Fatalf("second response=%#v", response)
	}
	want := []string{"Bearer first-token", "Bearer rotated-token"}
	if len(authorizations) != len(want) || authorizations[0] != want[0] || authorizations[1] != want[1] {
		t.Fatalf("authorizations=%v want=%v", authorizations, want)
	}
}

func TestAdapterRelayDoesNotCallLocalServiceWhenTokenReloadFails(t *testing.T) {
	var calls atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer local.Close()
	relay := adapterRelay{
		localBaseURL:   local.URL,
		loadLocalToken: func() (string, error) { return "", errors.New("token rotated badly") },
		client:         local.Client(),
	}
	response := relay.handle(context.Background(), adapterRelayRequest{
		Type: "request", ID: "request-1", Method: http.MethodGet, Path: "/v1/workspaces/fleet-a-is-101",
	})
	if response.Status != http.StatusBadGateway || calls.Load() != 0 || !strings.Contains(response.Body, "adapter_auth_unavailable") {
		t.Fatalf("response=%#v calls=%d", response, calls.Load())
	}
}

func TestAdapterRelayDesktopTimeoutCoversConnectionSetup(t *testing.T) {
	desktop := adapterRelayRequest{Method: http.MethodPost, Path: "/v1/workspaces/fleet-a-is-101/connections/desktop"}
	if got := adapterRelayTimeoutForRequest(desktop, 11*time.Minute); got != 11*time.Minute {
		t.Fatalf("desktop timeout=%s", got)
	}
	ordinary := adapterRelayRequest{Method: http.MethodGet, Path: "/v1/workspaces/fleet-a-is-101"}
	if got := adapterRelayTimeoutForRequest(ordinary, 11*time.Minute); got != 9*time.Second {
		t.Fatalf("ordinary timeout=%s", got)
	}
}

func TestConfiguredAdapterCoordinatorClientAcceptsTokenCommand(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CRABBOX_COORDINATOR", "https://coordinator.example")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN_COMMAND", `["token-helper","--scope","adapter"]`)
	coord, err := configuredAdapterCoordinatorClient()
	if err != nil {
		t.Fatalf("token-command coordinator rejected: %v", err)
	}
	if coord == nil || strings.Join(coord.TokenCommand, " ") != "token-helper --scope adapter" {
		t.Fatalf("coordinator=%#v", coord)
	}
}

func TestAdapterRelayServesDeleteWhileDesktopRequestIsInFlight(t *testing.T) {
	desktopStarted := make(chan struct{})
	releaseDesktop := make(chan struct{})
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/connections/desktop"):
			close(desktopStarted)
			<-releaseDesktop
			_, _ = io.WriteString(w, `{"url":"https://desktop.example"}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"status":"stopping"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer local.Close()

	serverErr := make(chan error, 1)
	coordinator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/adapters/mac-lab/ticket":
			_, _ = io.WriteString(w, `{"ticket":"adapter-ticket"}`)
		case "/v1/adapters/mac-lab/agent":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				serverErr <- err
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "test complete")
			release := func() {
				select {
				case <-releaseDesktop:
				default:
					close(releaseDesktop)
				}
			}
			defer release()
			requests := []string{
				`{"type":"request","id":"desktop","method":"POST","path":"/v1/workspaces/fleet-a-is-101/connections/desktop"}`,
				`{"type":"request","id":"delete","method":"DELETE","path":"/v1/workspaces/fleet-a-is-101"}`,
			}
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(requests[0])); err != nil {
				serverErr <- err
				return
			}
			select {
			case <-desktopStarted:
			case <-r.Context().Done():
				serverErr <- r.Context().Err()
				return
			}
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(requests[1])); err != nil {
				serverErr <- err
				return
			}
			_, data, err := conn.Read(r.Context())
			if err != nil {
				serverErr <- err
				return
			}
			var first adapterRelayResponse
			if err := json.Unmarshal(data, &first); err != nil {
				serverErr <- err
				return
			}
			if first.ID != "delete" || first.Status != http.StatusAccepted {
				serverErr <- errors.New("delete was blocked behind desktop setup")
				return
			}
			release()
			_, data, err = conn.Read(r.Context())
			if err != nil {
				serverErr <- err
				return
			}
			var second adapterRelayResponse
			if err := json.Unmarshal(data, &second); err != nil {
				serverErr <- err
				return
			}
			if second.ID != "desktop" || second.Status != http.StatusOK {
				serverErr <- errors.New("desktop response missing after cancellation request")
				return
			}
			serverErr <- nil
		default:
			http.NotFound(w, r)
		}
	}))
	defer coordinator.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := connectAdapterRelay(ctx, &CoordinatorClient{
		BaseURL: coordinator.URL,
		Token:   "coordinator-token",
		Client:  coordinator.Client(),
	}, "mac-lab", local.URL, "/test/adapter.sock", func() (string, error) {
		return "local-token", nil
	}, local.Client(), 150*time.Second, io.Discard)
	var closeError websocket.CloseError
	if err == nil || !errors.As(err, &closeError) || closeError.Code != websocket.StatusNormalClosure {
		t.Fatalf("relay close error=%v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestConnectAdapterRelayUsesTicketHeaderAndRelaysResponse(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/workspaces/fleet-a-is-101" {
			t.Errorf("local request=%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer local-token" {
			t.Errorf("local authorization=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fleet-a-is-101","status":"ready"}`)
	}))
	defer local.Close()

	result := make(chan adapterRelayResponse, 1)
	coordinator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/adapters/mac-lab/ticket":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer coordinator-token" {
				t.Errorf("ticket request method=%s auth=%q", r.Method, r.Header.Get("Authorization"))
			}
			if r.Header.Get("CF-Access-Client-Id") != "access-client" || r.Header.Get("CF-Access-Client-Secret") != "access-secret" || r.Header.Get("cf-access-token") != "access-token" {
				t.Errorf("ticket request missing Access credentials: %#v", r.Header)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ticket":"adapter-ticket","adapterID":"mac-lab"}`)
		case "/v1/adapters/mac-lab/agent":
			if got := r.Header.Get("Authorization"); got != "Bearer adapter-ticket" {
				t.Errorf("agent authorization=%q", got)
			}
			if r.Header.Get("CF-Access-Client-Id") != "access-client" || r.Header.Get("CF-Access-Client-Secret") != "access-secret" || r.Header.Get("cf-access-token") != "access-token" {
				t.Errorf("agent handshake missing Access credentials: %#v", r.Header)
			}
			if r.URL.RawQuery != "" {
				t.Errorf("agent ticket leaked into query: %q", r.URL.RawQuery)
			}
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept adapter websocket: %v", err)
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "test complete")
			request := `{"type":"request","id":"request-1","method":"GET","path":"/v1/workspaces/fleet-a-is-101","headers":{"authorization":"Bearer remote-token"}}`
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(request)); err != nil {
				t.Errorf("write adapter request: %v", err)
				return
			}
			_, data, err := conn.Read(r.Context())
			if err != nil {
				t.Errorf("read adapter response: %v", err)
				return
			}
			var response adapterRelayResponse
			if err := json.Unmarshal(data, &response); err != nil {
				t.Errorf("decode adapter response: %v", err)
				return
			}
			result <- response
		default:
			http.NotFound(w, r)
		}
	}))
	defer coordinator.Close()

	baseCoordinatorClient := coordinator.Client()
	var coordinatorRequests atomic.Int32
	coordinatorClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		coordinatorRequests.Add(1)
		return baseCoordinatorClient.Transport.RoundTrip(request)
	})}
	coord := &CoordinatorClient{
		BaseURL: coordinator.URL,
		Token:   "coordinator-token",
		Client:  coordinatorClient,
		Access:  AccessConfig{ClientID: "access-client", ClientSecret: "access-secret", Token: "access-token"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := connectAdapterRelay(
		ctx,
		coord,
		"mac-lab",
		local.URL,
		"/test/adapter.sock",
		func() (string, error) { return "local-token", nil },
		local.Client(),
		150*time.Second,
		io.Discard,
	)
	var closeError websocket.CloseError
	if err == nil || !errors.As(err, &closeError) || closeError.Code != websocket.StatusNormalClosure {
		t.Fatalf("relay close error=%v", err)
	}
	select {
	case response := <-result:
		if response.Type != "response" || response.ID != "request-1" || response.Status != http.StatusOK || response.Body != `{"id":"fleet-a-is-101","status":"ready"}` {
			t.Fatalf("relayed response=%#v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not receive adapter response")
	}
	if got := coordinatorRequests.Load(); got != 2 {
		t.Fatalf("configured coordinator transport requests=%d want ticket+websocket", got)
	}
}

func TestConnectAdapterRelayBoundsTicketAcquisition(t *testing.T) {
	var deadline time.Time
	coord := &CoordinatorClient{
		BaseURL: "https://coordinator.example",
		Token:   "coordinator-token",
		Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			var ok bool
			deadline, ok = request.Context().Deadline()
			if !ok {
				t.Fatal("ticket request did not carry the relay handshake deadline")
			}
			return nil, errors.New("stop after deadline inspection")
		})},
	}
	started := time.Now()
	err := connectAdapterRelay(
		context.Background(), coord, "mac-lab", "http://127.0.0.1", "/test/adapter.sock",
		func() (string, error) { return "local-token", nil }, http.DefaultClient, 150*time.Second, io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "create adapter ticket") {
		t.Fatalf("relay ticket error=%v", err)
	}
	remaining := deadline.Sub(started)
	if remaining <= 0 || remaining > adapterRelayHandshakeTimeout+time.Second {
		t.Fatalf("ticket deadline remaining=%s handshake timeout=%s", remaining, adapterRelayHandshakeTimeout)
	}
}

func TestAdapterRelayLoopStopsCleanlyWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := runAdapterRelayLoop(ctx, io.Discard, func(context.Context) error {
		calls++
		cancel()
		return errors.New("disconnected")
	})
	if err != nil || calls != 1 {
		t.Fatalf("loop error=%v calls=%d", err, calls)
	}
}

func TestAdapterRelayBackoffIsBounded(t *testing.T) {
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for attempt, expected := range want {
		if got := adapterRelayBackoff(attempt); got != expected {
			t.Errorf("attempt=%d delay=%s want=%s", attempt, got, expected)
		}
	}
}
