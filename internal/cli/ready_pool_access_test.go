package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestPortablePoolKeyIsUniquePrivateAndDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	first, publicKey, err := createPoolAccessKey(filepath.Join(dir, "one.json"))
	if err != nil {
		t.Fatal(err)
	}
	second, another, err := createPoolAccessKey(filepath.Join(dir, "two.json"))
	if err != nil {
		t.Fatal(err)
	}
	if publicKey == another {
		t.Fatal("reused ephemeral key")
	}
	for _, path := range []string{first, second} {
		info, err := os.Stat(path + ".key")
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private key mode: %v %v", info, err)
		}
		data, err := os.ReadFile(path + ".key")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = ssh.ParsePrivateKey(data); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err = createPoolAccessKey(first); err == nil {
		t.Fatal("overwrote receipt")
	}
	if err = os.Chmod(first, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = readPoolAccessReceipt(first); err == nil {
		t.Fatal("accepted exposed receipt")
	}
}

func TestPortablePoolBorrowReceiptAndImmutableAcknowledgement(t *testing.T) {
	for _, changedDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "changed expiry"}[changedDeadline], func(t *testing.T) {
			identity := testReadyPoolIdentity(t, "", "", "", "")
			deadline := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
			path := filepath.Join(t.TempDir(), "receipt.json")
			var publicKey string
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.URL.Path)
				var input map[string]any
				_ = json.NewDecoder(r.Body).Decode(&input)
				grant := &CoordinatorPoolGrant{ID: "grant-1", LeaseID: "cbx_000000000099", State: "pending", ExpiresAt: deadline, Generation: 1}
				res := CoordinatorReadyPoolResponse{Entry: CoordinatorReadyPoolEntry{Key: "builders", LeaseID: grant.LeaseID, Identity: &identity}, Grant: grant,
					Lease: CoordinatorLease{ID: grant.LeaseID, Provider: "aws", Region: "us-east-1", TargetOS: targetLinux, Architecture: "amd64", Image: &CoordinatorLeaseImage{ID: identity.Image.ID, Provider: "aws", Kind: "aws-ami", Region: "us-east-1"}}}
				if strings.HasSuffix(r.URL.Path, "borrow-access") {
					publicKey, _ = input["publicKey"].(string)
					if !strings.HasPrefix(publicKey, "ssh-ed25519 ") {
						t.Error("no public key")
					}
					res.BorrowToken = "synthetic-borrow-token"
					res.ReceiptToken = "synthetic-receipt-token"
				} else if strings.HasSuffix(r.URL.Path, "ack-access") {
					saved, err := readPoolAccessReceipt(path)
					if err != nil || saved.ReceiptToken != "synthetic-receipt-token" {
						t.Errorf("ack before durable receipt: %v", err)
					}
					if input["publicKey"] != publicKey {
						t.Error("ack key changed")
					}
					grant.State = "active"
					if changedDeadline {
						grant.ExpiresAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
					}
				} else {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				res.Grant.Fingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte(publicKey)))
				_ = json.NewEncoder(w).Encode(res)
			}))
			defer server.Close()
			client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
			var log bytes.Buffer
			res, receipt, err := borrowPortablePool(context.Background(), client, "builders", map[string]any{}, identity, path, 30*time.Minute, &log)
			if changedDeadline && err == nil {
				t.Fatal("accepted renewed deadline")
			}
			if !changedDeadline && err != nil {
				t.Fatal(err)
			}
			if receipt != path || len(calls) != 2 {
				t.Fatalf("receipt=%s calls=%v", receipt, calls)
			}
			if !changedDeadline && (res.BorrowToken != "" || res.ReceiptToken != "" || res.Entry.BorrowToken != "") {
				t.Fatal("public output contains tokens")
			}
			if strings.Contains(log.String(), "synthetic-borrow-token") || strings.Contains(log.String(), "synthetic-receipt-token") || strings.Contains(log.String(), "PRIVATE KEY") {
				t.Fatal("secret in output")
			}
		})
	}
}

func TestPortablePoolRetainsReceiptUntilConfirmedFence(t *testing.T) {
	path, publicKey, err := createPoolAccessKey(filepath.Join(t.TempDir(), "receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	receipt := poolAccessReceipt{GrantID: "grant", Generation: 1, Schema: "crabbox-pool-access-receipt/v1", Pool: "builders", LeaseID: "cbx_000000000099", BorrowToken: "borrow", ReceiptToken: "receipt", PublicKey: publicKey}
	if err = writePoolAccessReceipt(path, receipt); err != nil {
		t.Fatal(err)
	}
	fenced := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "return-access") {
			t.Error("legacy fallback")
		}
		if !fenced {
			http.Error(w, "fencing failed", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(CoordinatorReadyPoolResponse{Grant: &CoordinatorPoolGrant{ID: "grant", Generation: 1, LeaseID: receipt.LeaseID, State: "revoked"}})
	}))
	defer server.Close()
	client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	if err = returnPortablePool(context.Background(), client, path, "ready"); err == nil {
		t.Fatal("lost cleanup failure")
	}
	if _, err = os.Stat(path + ".key"); err != nil {
		t.Fatal("removed retry key")
	}
	fenced = true
	if err = returnPortablePool(context.Background(), client, path, "ready"); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{path, path + ".key"} {
		if _, err = os.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("retained terminal key/receipt")
		}
	}
}

func TestPortablePoolUnsupportedServerDoesNotFallback(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.NotFound(w, r) }))
	defer server.Close()
	client := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	path := filepath.Join(t.TempDir(), "receipt.json")
	_, _, err := borrowPortablePool(context.Background(), client, "builders", map[string]any{}, testReadyPoolIdentity(t, "", "", "", ""), path, 30*time.Minute, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unsupported") || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	if _, err = os.Stat(path + ".key"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("left key after rejected issue")
	}
}

func TestPortablePoolRunRequiresExplicitTypedOptIn(t *testing.T) {
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).runCommand(context.Background(), []string{"--pool-access", "--pool", "builders", "--", "true"})
	if err == nil || !strings.Contains(err.Error(), "--pool-identity-file") {
		t.Fatalf("got %v", err)
	}
}
