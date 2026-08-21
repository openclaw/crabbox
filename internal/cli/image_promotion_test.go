package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func writePromotionEvidence(t *testing.T, qualificationRef string) string {
	t.Helper()
	file := t.TempDir() + "/promotion-evidence.json"
	data := []byte(`{"schema":"crabbox-image-promotion-evidence/v1","qualification":{"reference":"` + qualificationRef + `"}}`)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestImagePromoteProtectedContract(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var request CoordinatorImagePromotionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/image-promotions" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != strings.Join([]string{"Bearer", "promotion-token"}, " ") {
			t.Fatalf("authorization=%q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, `{"image":{"id":"ami-qualified","name":"qualified","state":"available","provider":"aws","region":"eu-west-1","revision":"rev-next"},"attempt":{"phase":"awaiting_verification"}}`)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_PROMOTION_TOKEN", "promotion-token")
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "wrong-admin-token")

	var out bytes.Buffer
	qualificationRef := "ghcr.io/example-org/crabbox-aws-image-qualifications@sha256:" + strings.Repeat("a", 64)
	err := (App{Stdout: &out, Stderr: io.Discard}).imagePromote(context.Background(), []string{
		"--qualification-ref", qualificationRef,
		"--promotion-evidence", writePromotionEvidence(t, qualificationRef),
		"--expected-current-image", "ami-current",
		"--expected-current-revision", "rev-current",
		"--idempotency-key", "run-123-attempt-1",
		"--workflow-run-id", "123",
		"--workflow-run-attempt", "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Schema != "crabbox-image-promotion-request/v1" || request.Operation != "promote" {
		t.Fatalf("request=%#v", request)
	}
	if request.Expected != (CoordinatorImageDefaultState{State: "present", ImageID: "ami-current", Revision: "rev-current"}) {
		t.Fatalf("expected=%#v", request.Expected)
	}
	if got, want := out.String(), "promote phase=awaiting_verification outcome=- image=ami-qualified state=available region=eu-west-1 revision=rev-next\n"; got != want {
		t.Fatalf("output=%q want=%q", got, want)
	}
}

func TestImagePromoteProtectedAbsentAndRollbackContract(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var request CoordinatorImagePromotionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, `{"image":{"id":"ami-previous","state":"available","revision":"rev-rollback"},"attempt":{"phase":"awaiting_verification"}}`)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_PROMOTION_TOKEN", "promotion-token")

	qualificationRef := "ghcr.io/example-org/proof@sha256:" + strings.Repeat("b", 64)
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).imagePromote(context.Background(), []string{
		"--qualification-ref", qualificationRef,
		"--promotion-evidence", writePromotionEvidence(t, qualificationRef),
		"--expected-current-image", "none",
		"--idempotency-key", "rollback-123",
		"--workflow-run-id", "123",
		"--workflow-run-attempt", "2",
		"--rollback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Operation != "rollback" || request.Expected != (CoordinatorImageDefaultState{State: "absent"}) {
		t.Fatalf("request=%#v", request)
	}
}

func TestImagePromoteProtectedRejectsPendingAndCompletedFailurePhases(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "pending",
			body: `{"attempt":{"phase":"mutating"}}`,
			want: "still pending in phase mutating",
		},
		{
			name: "completed failure",
			body: `{"attempt":{"phase":"completed","outcome":"outcome_unknown"}}`,
			want: "completed with outcome outcome_unknown",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_PROMOTION_TOKEN", "promotion-token")
			qualificationRef := "ghcr.io/example-org/proof@sha256:" + strings.Repeat("e", 64)
			err := (App{Stdout: io.Discard, Stderr: io.Discard}).imagePromote(
				context.Background(),
				[]string{
					"--qualification-ref", qualificationRef,
					"--promotion-evidence", writePromotionEvidence(t, qualificationRef),
					"--expected-current-image", "none",
					"--idempotency-key", "pending-key",
					"--workflow-run-id", "123",
					"--workflow-run-attempt", "1",
				},
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestImagePromoteProtectedRejectsUnsafeOptionsBeforeRequest(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_PROMOTION_TOKEN", "promotion-token")

	qualificationRef := "ghcr.io/example-org/proof@sha256:" + strings.Repeat("c", 64)
	base := []string{
		"--qualification-ref", qualificationRef,
		"--promotion-evidence", writePromotionEvidence(t, qualificationRef),
		"--expected-current-image", "none",
		"--idempotency-key", "key",
		"--workflow-run-id", "123",
		"--workflow-run-attempt", "1",
	}
	for _, extra := range [][]string{
		{"--catalog-only"},
		{"--fast-snapshot-restore"},
		{"--fsr-az", "eu-west-1a"},
		{"--provider", "azure"},
		{"--region", "eu-west-1"},
		{"ami-operator-input"},
	} {
		args := append(append([]string{}, base...), extra...)
		err := (App{Stdout: io.Discard, Stderr: io.Discard}).imagePromote(context.Background(), args)
		if err == nil {
			t.Fatalf("args=%v unexpectedly succeeded", extra)
		}
	}
	if requests != 0 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestImagePromoteProtectedRequiresRevisionForPresentState(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	qualificationRef := "ghcr.io/example-org/proof@sha256:" + strings.Repeat("d", 64)
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).imagePromote(context.Background(), []string{
		"--qualification-ref", qualificationRef,
		"--promotion-evidence", writePromotionEvidence(t, qualificationRef),
		"--expected-current-image", "ami-current",
		"--idempotency-key", "key",
		"--workflow-run-id", "123",
		"--workflow-run-attempt", "1",
	})
	if err == nil || !strings.Contains(err.Error(), "--expected-current-revision is required") {
		t.Fatalf("error=%v", err)
	}
}

func TestImageDefaultStateAndAzureCASUsePromotionOnlyContract(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != strings.Join([]string{"Bearer", "promotion-token"}, " ") {
			t.Fatalf("authorization=%q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/image-promotions/default-state":
			if r.URL.Query().Get("provider") != "azure" || r.URL.Query().Get("serverType") != "Standard_D4s_v5" {
				t.Fatalf("query=%v", r.URL.Query())
			}
			_, _ = io.WriteString(w, `{"provider":"azure","scope":{"provider":"azure","target":"linux","os":"ubuntu:26.04","region":"westeurope","architecture":"amd64","serverType":"Standard_D4s_v5"},"state":{"state":"present","imageId":"snapshot-a","revision":"revision-a"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/image-promotions/provider-defaults":
			var body struct {
				Schema   string                       `json:"schema"`
				Provider string                       `json:"provider"`
				ImageID  string                       `json:"imageId"`
				Expected CoordinatorImageDefaultState `json:"expected"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Schema != "crabbox-provider-image-default-cas/v1" || body.Provider != "azure" ||
				body.ImageID != "snapshot-b" || body.Expected != (CoordinatorImageDefaultState{State: "present", ImageID: "snapshot-a", Revision: "revision-a"}) {
				t.Fatalf("body=%#v", body)
			}
			_, _ = io.WriteString(w, `{"image":{"id":"snapshot-b","state":"succeeded","provider":"azure","region":"westeurope","revision":"revision-b"}}`)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_PROMOTION_TOKEN", "promotion-token")
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "wrong-admin-token")

	common := []string{
		"--provider", "azure",
		"--target", "linux",
		"--os", "ubuntu:26.04",
		"--region", "westeurope",
		"--architecture", "amd64",
		"--type", "Standard_D4s_v5",
	}
	var stateOut bytes.Buffer
	if err := (App{Stdout: &stateOut, Stderr: io.Discard}).imageDefaultState(context.Background(), common); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stateOut.String(), "image=snapshot-a revision=revision-a") {
		t.Fatalf("state output=%q", stateOut.String())
	}

	args := append([]string{"snapshot-b"}, common...)
	args = append(args,
		"--expected-current-image", "snapshot-a",
		"--expected-current-revision", "revision-a",
	)
	var casOut bytes.Buffer
	if err := (App{Stdout: &casOut, Stderr: io.Discard}).imageCAS(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(casOut.String(), "revision=revision-b") || requests != 2 {
		t.Fatalf("cas output=%q requests=%d", casOut.String(), requests)
	}
}
