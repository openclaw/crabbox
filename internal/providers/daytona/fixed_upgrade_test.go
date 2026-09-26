package daytona

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	api "github.com/daytona/clients/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
)

func TestDaytonaFixedCleanupResumesPreMigrationClaims(t *testing.T) {
	for name, record := range preMigrationDaytonaDeletionClaims {
		for _, state := range []string{"pending", "error", "absent"} {
			t.Run(name+"/"+state, func(t *testing.T) {
				f, b, repo := newDaytonaLifecycleFixture(t)
				scope, _, err := fixedDaytonaScope(f.server.URL, "org-test")
				if err != nil {
					t.Fatal(err)
				}
				root, err := json.Marshal(repo.Root)
				if err != nil {
					t.Fatal(err)
				}
				data := strings.ReplaceAll(strings.ReplaceAll(record, "PROVIDER_SCOPE", scope), `"REPO_ROOT"`, string(root))
				var before core.LeaseClaim
				if err := json.Unmarshal([]byte(data), &before); err != nil {
					t.Fatal(err)
				}
				dir, err := core.CrabboxStateDir()
				if err != nil {
					t.Fatal(err)
				}
				dir = filepath.Join(dir, "claims")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, before.LeaseID+".json"), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				f.sandbox = &api.Sandbox{Id: before.CloudID, OrganizationId: "org-test", Labels: before.Labels}
				f.sandbox.SetDesiredState(api.SANDBOXDESIREDSTATE_DESTROYED)
				f.sandbox.SetState(api.SANDBOXSTATE_DESTROYING)
				if state == "error" {
					f.sandbox.SetState(api.SANDBOXSTATE_ERROR)
				} else if state == "absent" {
					f.sandbox = nil
				}
				var exactReads, accountReads atomic.Int32
				original := f.server.Config.Handler
				f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/sandbox/"+before.CloudID:
						exactReads.Add(1)
						if state == "absent" {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusNotFound)
							_ = json.NewEncoder(w).Encode(map[string]any{"path": r.URL.RequestURI(), "statusCode": 404, "error": "Not Found", "message": "Sandbox not found"})
							return
						}
					case r.Method == http.MethodGet && r.URL.Path == "/api-keys/current":
						accountReads.Add(1)
					default:
						t.Errorf("resumed cleanup issued unexpected request: %s %s", r.Method, r.URL.Path)
						http.Error(w, "legacy inventory and mutations unavailable", http.StatusGone)
						return
					}
					original.ServeHTTP(w, r)
				})
				stop := func(ctx context.Context) error { return b.Stop(ctx, core.StopRequest{ID: before.LeaseID}) }
				if state == "pending" {
					err = interruptDaytonaFixedCleanup(t, f, before.LeaseID, stop)
				} else {
					err = stop(t.Context())
				}
				after, exists, readErr := core.ReadLeaseClaimWithPresence(before.LeaseID)
				if readErr != nil || !exists || after.FixedCreateIntent == nil {
					t.Fatalf("historical claim lost: exists=%t err=%v", exists, readErr)
				}
				if (err == nil) != (state == "absent") || (after.FixedCreateIntent.State == "released") != (state == "absent") {
					t.Fatalf("incorrect upgrade outcome: err=%v state=%s", err, after.FixedCreateIntent.State)
				}
				if state == "error" && !strings.Contains(err.Error(), "deletion failed with state=error") {
					t.Fatalf("failed before observing deletion failure: %v", err)
				}
				if state != "absent" && (after.CloudID != before.CloudID || after.FixedCreateIntent.Attempt["deletion_indexed_id"] != before.CloudID || after.FixedCreateIntent.Attempt["deletion_acknowledged_id"] != before.CloudID) {
					t.Fatal("in-flight upgrade lost its resource or deletion witnesses")
				}
				if exactReads.Load() == 0 || accountReads.Load() == 0 {
					t.Fatal("resumed cleanup did not attest the account and exact resource")
				}
			})
		}
	}
}

// Captured from 0bace82d's real Acquire followed by Stop with a lost DELETE
// response (entry), or ReleaseLease interrupted after acknowledgment. Only
// the HTTP endpoint scope and repository directory are replaced per test.
// Keep these wire records independent of the current claim producer.
var preMigrationDaytonaDeletionClaims = map[string]string{
	"entry": `{
  "claimedAt": "2026-09-26T07:31:00Z",
  "cloudID": "sandbox-test",
  "cloudImmutableID": "sandbox-test",
  "fixedCreateIntent": {
    "attempt": {
      "deletion_indexed_id": "sandbox-test",
      "name": "crabbox-fixed-project-d57488f1",
      "nonce": "TIN64IV5HEZTI6CYZATTUFCRBT",
      "organization": "org-test",
      "resolved_target": "us",
      "snapshot": "test-snapshot",
      "snapshot_id": "snapshot-exact-id",
      "target": "",
      "user": "daytona"
    },
    "createdAt": "2026-09-26T07:31:00.465061Z",
    "fingerprint": "65961bb2195472b62d5b41448c8bf14ff452086b94825223c769639a7344e220",
    "journal": {
      "phase": "deleting",
      "revision": 7,
      "version": 1
    },
    "providerScope": "PROVIDER_SCOPE",
    "slug": "fixed-project",
    "state": "acquired",
    "version": 1
  },
  "idleTimeoutSeconds": 1800,
  "labels": {
    "crabbox": "true",
    "created_at": "1790407860",
    "created_by": "crabbox",
    "expires_at": "1790409660",
    "fixed_attempt": "TIN64IV5HEZTI6CYZATTUFCRBT",
    "fixed_claim_provider": "daytona-fixed-v1",
    "fixed_intent_sha256": "65961bb2195472b62d5b41448c8bf14ff452086b94825223c769639a7344e220",
    "idle_timeout": "1800",
    "idle_timeout_secs": "1800",
    "keep": "true",
    "last_touched_at": "1790407860",
    "lease": "cbx_012345abcdef",
    "lease_name": "crabbox-fixed-project-d57488f1",
    "profile": "default",
    "provider": "daytona",
    "provider_key": "fixture-key",
    "server_type": "unknown",
    "slug": "fixed-project",
    "state": "leased",
    "target": "linux",
    "ttl_secs": "5400",
    "work_root": "/work/upgrade"
  },
  "lastUsedAt": "2026-09-26T07:31:01Z",
  "leaseID": "cbx_012345abcdef",
  "provider": "daytona-fixed-v1",
  "providerScope": "PROVIDER_SCOPE",
  "repoRoot": "REPO_ROOT",
  "revision": "0a95de019838c722c881c278469cd513",
  "slug": "fixed-project",
  "sshHost": "127.0.0.1",
  "sshPort": 57592,
  "targetOS": "linux"
}`,
	"acknowledged": `{
  "claimedAt": "2026-09-26T07:31:01Z",
  "cloudID": "sandbox-test",
  "cloudImmutableID": "sandbox-test",
  "fixedCreateIntent": {
    "attempt": {
      "deletion_acknowledged_id": "sandbox-test",
      "deletion_indexed_id": "sandbox-test",
      "name": "crabbox-fixed-project-d57488f1",
      "nonce": "7ZV3CJLKPOVOVJQAHYO7KQKRZM",
      "organization": "org-test",
      "resolved_target": "us",
      "snapshot": "test-snapshot",
      "snapshot_id": "snapshot-exact-id",
      "target": "",
      "user": "daytona"
    },
    "createdAt": "2026-09-26T07:31:01.605024Z",
    "fingerprint": "65961bb2195472b62d5b41448c8bf14ff452086b94825223c769639a7344e220",
    "journal": {
      "phase": "deleting",
      "revision": 8,
      "version": 1
    },
    "providerScope": "PROVIDER_SCOPE",
    "slug": "fixed-project",
    "state": "acquired",
    "version": 1
  },
  "idleTimeoutSeconds": 1800,
  "labels": {
    "crabbox": "true",
    "created_at": "1790407861",
    "created_by": "crabbox",
    "expires_at": "1790409661",
    "fixed_attempt": "7ZV3CJLKPOVOVJQAHYO7KQKRZM",
    "fixed_claim_provider": "daytona-fixed-v1",
    "fixed_intent_sha256": "65961bb2195472b62d5b41448c8bf14ff452086b94825223c769639a7344e220",
    "idle_timeout": "1800",
    "idle_timeout_secs": "1800",
    "keep": "true",
    "last_touched_at": "1790407861",
    "lease": "cbx_012345abcdef",
    "lease_name": "crabbox-fixed-project-d57488f1",
    "profile": "default",
    "provider": "daytona",
    "provider_key": "fixture-key",
    "server_type": "unknown",
    "slug": "fixed-project",
    "state": "leased",
    "target": "linux",
    "ttl_secs": "5400",
    "work_root": "/work/upgrade"
  },
  "lastUsedAt": "2026-09-26T07:31:02Z",
  "leaseID": "cbx_012345abcdef",
  "provider": "daytona-fixed-v1",
  "providerScope": "PROVIDER_SCOPE",
  "repoRoot": "REPO_ROOT",
  "revision": "d260403c0f85f2ff5e713bf47c915209",
  "slug": "fixed-project",
  "sshHost": "127.0.0.1",
  "sshPort": 57725,
  "targetOS": "linux"
}`,
}
