package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImagePromoteProtectedContract(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var request CoordinatorImagePromotionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/image-promotions" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer promotion-token" {
			t.Fatalf("authorization=%q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, `{"image":{"id":"ami-qualified","name":"qualified","state":"available","provider":"aws","region":"eu-west-1","revision":"rev-next"}}`)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_PROMOTION_TOKEN", "promotion-token")
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "wrong-admin-token")

	var out bytes.Buffer
	err := (App{Stdout: &out, Stderr: io.Discard}).imagePromote(context.Background(), []string{
		"--qualification-ref", "ghcr.io/example-org/crabbox-aws-image-qualifications@sha256:" + strings.Repeat("a", 64),
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
	if got, want := out.String(), "promote image=ami-qualified state=available region=eu-west-1 revision=rev-next\n"; got != want {
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
		_, _ = io.WriteString(w, `{"image":{"id":"ami-previous","state":"available","revision":"rev-rollback"}}`)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_PROMOTION_TOKEN", "promotion-token")

	err := (App{Stdout: io.Discard, Stderr: io.Discard}).imagePromote(context.Background(), []string{
		"--qualification-ref", "ghcr.io/example-org/proof@sha256:" + strings.Repeat("b", 64),
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

	base := []string{
		"--qualification-ref", "ghcr.io/example-org/proof@sha256:" + strings.Repeat("c", 64),
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
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).imagePromote(context.Background(), []string{
		"--qualification-ref", "ghcr.io/example-org/proof@sha256:" + strings.Repeat("d", 64),
		"--expected-current-image", "ami-current",
		"--idempotency-key", "key",
		"--workflow-run-id", "123",
		"--workflow-run-attempt", "1",
	})
	if err == nil || !strings.Contains(err.Error(), "--expected-current-revision is required") {
		t.Fatalf("error=%v", err)
	}
}
