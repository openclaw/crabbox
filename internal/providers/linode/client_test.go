package linode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestLinodeClientCreateRequestShape(t *testing.T) {
	var captured struct {
		Method      string
		Path        string
		Auth        string
		Accept      string
		ContentType string
		Body        map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Method = r.Method
		captured.Path = r.URL.RequestURI()
		captured.Auth = r.Header.Get("Authorization")
		captured.Accept = r.Header.Get("Accept")
		captured.ContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&captured.Body); err != nil {
			t.Fatal(err)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/linode/instances" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":123,"label":"crabbox-cbx-test","status":"provisioning","region":"us-ord","type":"g6-standard-1","image":"linode/ubuntu24.04","tags":["crabbox"]}`))
	}))
	defer server.Close()

	t.Setenv(tokenEnv, "secret-token")
	client, err := newLinodeClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	got, err := client.CreateLinode(context.Background(), createLinodeRequest{
		Region:         "us-ord",
		Type:           "g6-standard-1",
		Image:          "linode/ubuntu24.04",
		Label:          "crabbox-cbx-test",
		Tags:           []string{"crabbox", "lease:cbx_test"},
		AuthorizedKeys: []string{"ssh-ed25519 test"},
		Metadata:       &linodeMetadata{UserData: "I2Nsb3VkLWNvbmZpZw=="},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 123 || got.Label != "crabbox-cbx-test" {
		t.Fatalf("instance=%#v", got)
	}
	if captured.Method != http.MethodPost || captured.Path != "/linode/instances" {
		t.Fatalf("request=%s %s", captured.Method, captured.Path)
	}
	if captured.Auth != "Bearer secret-token" || captured.Accept != "application/json" || !strings.HasPrefix(captured.ContentType, "application/json") {
		t.Fatalf("headers auth=%q accept=%q content-type=%q", captured.Auth, captured.Accept, captured.ContentType)
	}
	if captured.Body["region"] != "us-ord" || captured.Body["type"] != "g6-standard-1" || captured.Body["image"] != "linode/ubuntu24.04" {
		t.Fatalf("body=%v", captured.Body)
	}
	keys, _ := captured.Body["authorized_keys"].([]any)
	if len(keys) != 1 || keys[0] != "ssh-ed25519 test" {
		t.Fatalf("authorized_keys=%v", captured.Body["authorized_keys"])
	}
	metadata, _ := captured.Body["metadata"].(map[string]any)
	if metadata["user_data"] != "I2Nsb3VkLWNvbmZpZw==" {
		t.Fatalf("metadata=%v", captured.Body["metadata"])
	}
}

func TestLinodeClientAccountID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/account" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
		_, _ = w.Write([]byte(`{"euuid":"A1BC2DEF-34GH-567I-J890KLMN12O34P56"}`))
	}))
	defer server.Close()

	t.Setenv(tokenEnv, "token")
	client, err := newLinodeClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	accountID, err := client.AccountID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if accountID != "euuid:A1BC2DEF-34GH-567I-J890KLMN12O34P56" {
		t.Fatalf("accountID=%q", accountID)
	}
}

func TestLinodeClientAccountSettings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/account/settings" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
		_, _ = w.Write([]byte(`{"interfaces_for_new_linodes":"linode_only"}`))
	}))
	defer server.Close()

	t.Setenv(tokenEnv, "token")
	client, err := newLinodeClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	settings, err := client.AccountSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.InterfacesForNewLinodes != "linode_only" {
		t.Fatalf("settings=%#v", settings)
	}
}

func TestLinodeClientUpdateTagsRequestShape(t *testing.T) {
	var captured struct {
		Method string
		Path   string
		Body   map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Method = r.Method
		captured.Path = r.URL.RequestURI()
		if err := json.NewDecoder(r.Body).Decode(&captured.Body); err != nil {
			t.Fatal(err)
		}
		if r.Method != http.MethodPut || r.URL.Path != "/linode/instances/123" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	t.Setenv(tokenEnv, "token")
	client, err := newLinodeClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	if err := client.UpdateLinodeTags(context.Background(), 123, []string{"crabbox", "crabbox:state:ready"}); err != nil {
		t.Fatal(err)
	}
	tags, _ := captured.Body["tags"].([]any)
	if captured.Method != http.MethodPut || captured.Path != "/linode/instances/123" || len(tags) != 2 || tags[1] != "crabbox:state:ready" {
		t.Fatalf("captured=%#v", captured)
	}
}

func TestLinodeClientPagination(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write([]byte(`{"data":[{"id":1,"label":"one"}],"page":1,"pages":2,"results":2}`))
		case "2":
			_, _ = w.Write([]byte(`{"data":[{"id":2,"label":"two"}],"page":2,"pages":2,"results":2}`))
		default:
			t.Fatalf("unexpected page path %s", r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv(tokenEnv, "token")
	client, err := newLinodeClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	linodes, err := client.ListLinodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(linodes) != 2 || linodes[0].ID != 1 || linodes[1].ID != 2 {
		t.Fatalf("linodes=%v", linodes)
	}
	if strings.Join(paths, ",") != "/linode/instances?page=1&page_size=500,/linode/instances?page=2&page_size=500" {
		t.Fatalf("paths=%v", paths)
	}

	t.Run("instances", func(t *testing.T) {
		testLinodePageContract(t, "/linode/instances", (*linodeClient).ListLinodes, linodeInstance{ID: 1, Label: "one"}, linodeInstance{ID: 2, Label: "two"})
	})
	t.Run("types", func(t *testing.T) {
		testLinodePageContract(t, "/linode/types", (*linodeClient).ListTypes, linodeType{ID: "one", Label: "one"}, linodeType{ID: "two", Label: "two"})
	})
	t.Run("images", func(t *testing.T) {
		testLinodePageContract(t, "/images", (*linodeClient).ListImages, linodeImage{ID: "one", Label: "one"}, linodeImage{ID: "two", Label: "two"})
	})
	t.Run("regions", func(t *testing.T) {
		testLinodePageContract(t, "/regions", (*linodeClient).ListRegions, linodeRegion{ID: "one", Label: "one"}, linodeRegion{ID: "two", Label: "two"})
	})
	t.Run("firewalls", func(t *testing.T) {
		testLinodePageContract(t, "/networking/firewalls", (*linodeClient).ListFirewalls, linodeFirewall{ID: 1, Label: "one"}, linodeFirewall{ID: 2, Label: "two"})
	})
}

func TestLinodeClientErrorRedaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errors":[{"reason":"token secret-token root_pass hunter2 user_data I2Nsb3VkLWNvbmZpZw=="}],"root_pass":"hunter2","user_data":"I2Nsb3VkLWNvbmZpZw=="}`, http.StatusBadRequest)
	}))
	defer server.Close()

	t.Setenv(tokenEnv, "secret-token")
	client, err := newLinodeClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	err = client.do(context.Background(), http.MethodPost, "/linode/instances", createLinodeRequest{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	text := err.Error()
	for _, secret := range []string{"secret-token", "hunter2", "I2Nsb3VkLWNvbmZpZw=="} {
		if strings.Contains(text, secret) {
			t.Fatalf("error leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "<redacted>") {
		t.Fatalf("error not redacted: %s", text)
	}
}

type envelopeRoundTripper func(*http.Request) (*http.Response, error)

func (f envelopeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type envelopeMarshaler func() ([]byte, error)

func (f envelopeMarshaler) MarshalJSON() ([]byte, error) { return f() }

func TestLinodeClientRequestEnvelope(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "envelope-context")
	var pointer *struct{ Value string }
	var slice []string
	for _, tc := range []struct {
		name    string
		body    any
		want    string
		nilBody bool
	}{
		{"nil interface", nil, "", true},
		{"typed nil pointer", pointer, "null\n", false},
		{"typed nil slice", slice, "null\n", false},
		{"JSON bytes and HTML escaping", map[string]string{"message": "<&>"}, "{\"message\":\"\\u003c\\u0026\\u003e\"}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			transport := envelopeRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Context() != ctx || req.Context().Value(contextKey{}) != "envelope-context" {
					t.Fatal("request lost caller context")
				}
				if req.Method != http.MethodPost || req.URL.String() != "https://api.example.test/v1/records?limit=2" {
					t.Fatalf("request=%s %s", req.Method, req.URL)
				}
				if req.Header.Get("Authorization") != "Bearer synthetic-envelope-token" || req.Header.Get("Accept") != "application/json" {
					t.Fatalf("headers=%v", req.Header)
				}
				contentType := "application/json"
				if tc.nilBody {
					contentType = ""
				}
				if req.Header.Get("Content-Type") != contentType {
					t.Fatalf("Content-Type=%q want %q", req.Header.Get("Content-Type"), contentType)
				}
				if (req.Body == nil) != tc.nilBody {
					t.Fatalf("nil body=%v want %v", req.Body == nil, tc.nilBody)
				}
				if req.ContentLength != int64(len(tc.want)) {
					t.Fatalf("ContentLength=%d want %d", req.ContentLength, len(tc.want))
				}
				if req.Body != nil {
					data, err := io.ReadAll(req.Body)
					if err != nil {
						t.Fatal(err)
					}
					if string(data) != tc.want {
						t.Fatalf("body=%q want %q", data, tc.want)
					}
				}
				if tc.nilBody {
					if req.GetBody != nil {
						t.Fatal("nil input gained GetBody")
					}
				} else {
					if req.GetBody == nil {
						t.Fatal("encoded body lost GetBody")
					}
					replay, err := req.GetBody()
					if err != nil {
						t.Fatal(err)
					}
					defer replay.Close()
					data, err := io.ReadAll(replay)
					if err != nil {
						t.Fatal(err)
					}
					if string(data) != tc.want {
						t.Fatalf("replay=%q want %q", data, tc.want)
					}
				}
				return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			})
			client := &linodeClient{token: "synthetic-envelope-token", baseURL: "https://api.example.test/v1", client: &http.Client{Transport: transport}}
			if err := client.do(ctx, http.MethodPost, "/records?limit=2", tc.body, nil); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("transport calls=%d", calls)
			}
		})
	}
	t.Run("encoding fails before request construction and transport", func(t *testing.T) {
		sentinel := errors.New("synthetic encoder failure")
		calls := 0
		client := &linodeClient{baseURL: ":invalid", client: &http.Client{Transport: envelopeRoundTripper(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected transport") })}}
		body := envelopeMarshaler(func() ([]byte, error) { return nil, sentinel })
		err := client.do(nil, "invalid method", "/records", body, nil)
		var marshalerError *json.MarshalerError
		if !errors.As(err, &marshalerError) || !errors.Is(err, sentinel) {
			t.Fatalf("error=%T %v want original encoding cause", err, err)
		}
		if calls != 0 {
			t.Fatalf("transport calls=%d", calls)
		}
	})
}

type paginationErrorReader struct{ err error }

func (r paginationErrorReader) Read([]byte) (int, error) { return 0, r.err }

func testLinodePageContract[T any](t *testing.T, path string, list func(*linodeClient, context.Context) ([]T, error), first, second T) {
	t.Helper()
	encode := func(v any) string {
		t.Helper()
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	page := func(data string, pages int) string {
		return `{"data":` + data + `,"page":99,"pages":` + strconv.Itoa(pages) + `,"results":0}`
	}
	firstPage := page(encode([]T{first}), 2)
	secondData := encode([]T{second})
	partialBad := `[` + encode(second) + `,{"id":[]}]`
	sentinel := errors.New("synthetic pagination read failure")
	type response struct {
		body      string
		status    int
		readError bool
	}
	for _, tc := range []struct {
		name      string
		responses []response
		want      []T
		errorKind string
	}{
		{"order and duplicates", []response{{body: page(encode([]T{first, second, first}), 2)}, {body: page(secondData, 2)}}, []T{first, second, first, second}, ""},
		{"empty first page continues despite results and response page", []response{{body: page("[]", 2)}, {body: page(secondData, 2)}}, []T{second}, ""},
		{"null first page continues", []response{{body: page("null", 2)}, {body: page(secondData, 2)}}, []T{second}, ""},
		{"empty terminal page remains nil", []response{{body: page("[]", 1)}}, nil, ""},
		{"null terminal page remains nil", []response{{body: page("null", 1)}}, nil, ""},
		{"omitted metadata empty data", []response{{body: `{"data":[]}`}}, nil, ""},
		{"null metadata empty data", []response{{body: `{"data":[],"page":null,"pages":null,"results":null}`}}, nil, ""},
		{"pages zero still appends current data", []response{{body: page(encode([]T{first}), 0)}}, []T{first}, ""},
		{"pages negative still appends current data", []response{{body: page(encode([]T{first}), -1)}}, []T{first}, ""},
		{"later HTTP failure retains prior", []response{{body: firstPage}, {body: "synthetic unavailable", status: 503}}, []T{first}, "http"},
		{"later data failure discards partial current page", []response{{body: firstPage}, {body: page(partialBad, 2)}}, []T{first}, "data"},
		{"decode data before terminal stop", []response{{body: page(partialBad, 0)}}, nil, "data"},
		{"missing data remains a decode failure", []response{{body: `{"page":1,"pages":0,"results":0}`}}, nil, "missing-data"},
		{"later invalid page metadata", []response{{body: firstPage}, {body: `{"data":` + secondData + `,"page":"bad","pages":2,"results":0}`}}, []T{first}, "metadata"},
		{"later invalid results metadata", []response{{body: firstPage}, {body: `{"data":` + secondData + `,"page":2,"pages":2,"results":"bad"}`}}, []T{first}, "metadata"},
		{"later invalid pages metadata", []response{{body: firstPage}, {body: `{"data":` + secondData + `,"page":2,"pages":"bad","results":0}`}}, []T{first}, "metadata"},
		{"later response read failure retains prior", []response{{body: firstPage}, {readError: true}}, []T{first}, "read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), struct{ key string }{"pagination"}, tc.name)
			calls := 0
			client := &linodeClient{baseURL: "https://api.example.test", token: "synthetic-page-token"}
			client.client = &http.Client{Transport: envelopeRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls > len(tc.responses) {
					t.Fatalf("unexpected additional page%d", calls)
				}
				wantPath := path + "?page=" + strconv.Itoa(calls) + "&page_size=500"
				if req.URL.RequestURI() != wantPath || req.Method != http.MethodGet || req.Body != nil || req.Context() != ctx {
					t.Fatalf("request=%s %s body=%v context preserved=%v", req.Method, req.URL, req.Body, req.Context() == ctx)
				}
				if req.Header.Get("Authorization") != "Bearer synthetic-page-token" || req.Header.Get("Accept") != "application/json" || req.Header.Get("Content-Type") != "" {
					t.Fatalf("headers=%v", req.Header)
				}
				response := tc.responses[calls-1]
				status := response.status
				if status == 0 {
					status = 200
				}
				var body io.Reader = strings.NewReader(response.body)
				if response.readError {
					body = paginationErrorReader{sentinel}
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(body), Request: req}, nil
			})}
			got, err := list(client, ctx)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("items=%#v want %#v", got, tc.want)
			}
			if calls != len(tc.responses) {
				t.Fatalf("calls=%d want %d", calls, len(tc.responses))
			}
			nextPath := path + "?page=" + strconv.Itoa(calls) + "&page_size=500"
			switch tc.errorKind {
			case "":
				if err != nil {
					t.Fatal(err)
				}
			case "http":
				var api *linodeAPIError
				if !errors.As(err, &api) || api.Status != 503 || api.Operation != "GET "+nextPath || api.Body != "synthetic unavailable" || err.Error() != "linode GET "+nextPath+": http 503: synthetic unavailable" {
					t.Fatalf("HTTP error=%T %v", err, err)
				}
			case "read":
				if !errors.Is(err, sentinel) || err.Error() != "linode GET "+nextPath+" response body: "+sentinel.Error() {
					t.Fatalf("read error=%T %v", err, err)
				}
			case "data", "missing-data":
				if err == nil {
					t.Fatal("missing data decode error")
				}
				cause := errors.Unwrap(err)
				if cause == nil || err.Error() != "linode "+nextPath+" decode data: "+cause.Error() {
					t.Fatalf("data wrapper=%T %v", err, err)
				}
				if tc.errorKind == "data" {
					var mismatch *json.UnmarshalTypeError
					if !errors.As(err, &mismatch) {
						t.Fatalf("data cause=%T", cause)
					}
				} else {
					var syntax *json.SyntaxError
					if !errors.As(err, &syntax) {
						t.Fatalf("missing-data cause=%T", cause)
					}
				}
			case "metadata":
				if err == nil {
					t.Fatal("missing metadata error")
				}
				cause := errors.Unwrap(err)
				var mismatch *json.UnmarshalTypeError
				if cause == nil || !errors.As(err, &mismatch) || err.Error() != "linode GET "+nextPath+" decode: "+cause.Error() {
					t.Fatalf("metadata wrapper=%T %v", err, err)
				}
			}
		})
	}
}
