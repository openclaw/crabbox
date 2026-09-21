package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type CoordinatorPoolGrant struct {
	ID                      string `json:"id"`
	State                   string `json:"state"`
	LeaseID                 string `json:"leaseID"`
	ExpiresAt               string `json:"expiresAt"`
	AcknowledgementDeadline string `json:"acknowledgementDeadline"`
	Generation              int    `json:"generation"`
	Fingerprint             string `json:"fingerprint"`
}

type poolAccessReceipt struct {
	GrantID      string                         `json:"grantID"`
	Generation   int                            `json:"generation"`
	Schema       string                         `json:"schema"`
	Pool         string                         `json:"pool"`
	LeaseID      string                         `json:"leaseID"`
	BorrowToken  string                         `json:"borrowToken"`
	ReceiptToken string                         `json:"receiptToken"`
	PublicKey    string                         `json:"publicKey"`
	Identity     CoordinatorReadyPoolIdentityV1 `json:"identity"`
	ExpiresAt    string                         `json:"expiresAt"`
}

func (c *CoordinatorClient) poolAccessCall(ctx context.Context, key, action string, body any, out any) error {
	err := c.do(ctx, http.MethodPost, "/v1/ready-pools/"+url.PathEscape(key)+"/"+action+"-access", body, out)
	if readyPoolCoordinatorRouteUnsupported(err) {
		return fmt.Errorf("portable ready-pool access is unsupported: %w", err)
	}
	return err
}

func createPoolAccessKey(receiptPath string) (string, string, error) {
	if receiptPath == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", "", err
		}
		dir, err := os.MkdirTemp(base, "crabbox-pool-access-")
		if err != nil {
			return "", "", err
		}
		receiptPath = filepath.Join(dir, "receipt.json")
	}
	receiptPath, err := filepath.Abs(receiptPath)
	if err != nil {
		return "", "", err
	}
	receiptFile, err := os.OpenFile(receiptPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", "", err
	}
	if err = receiptFile.Close(); err != nil {
		return "", "", err
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		_ = os.Remove(receiptPath)
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "crabbox bounded borrow")
	if err != nil {
		_ = os.Remove(receiptPath)
		return "", "", err
	}
	file, err := os.OpenFile(receiptPath+".key", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		_ = os.Remove(receiptPath)
		return "", "", err
	}
	_, writeErr := file.Write(pem.EncodeToMemory(block))
	err = errors.Join(writeErr, file.Close())
	if err != nil {
		_ = os.Remove(receiptPath)
		_ = os.Remove(receiptPath + ".key")
		return "", "", err
	}
	publicKey, err := ssh.NewPublicKey(privateKey.Public())
	if err != nil {
		return "", "", err
	}
	return receiptPath, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey))), nil
}

func writePoolAccessReceipt(path string, receipt poolAccessReceipt) error {
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return writeStateFileAtomic(path, data, syncControllerDirectory)
}

func readPoolAccessReceipt(path string) (poolAccessReceipt, error) {
	var receipt poolAccessReceipt
	info, err := os.Lstat(path)
	if err != nil {
		return receipt, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return receipt, Exit(2, "access receipt must be an owner-only regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return receipt, err
	}
	if err = json.Unmarshal(data, &receipt); err != nil {
		return receipt, err
	}
	if receipt.Schema != "crabbox-pool-access-receipt/v1" || receipt.LeaseID == "" || receipt.BorrowToken == "" || receipt.ReceiptToken == "" {
		return receipt, Exit(2, "invalid portable pool receipt")
	}
	return receipt, nil
}

func removePoolAccessReceipt(path string) error {
	var result error
	for _, file := range []string{path + ".key", path} {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func borrowPortablePool(ctx context.Context, coord *CoordinatorClient, key string, input map[string]any, identity CoordinatorReadyPoolIdentityV1, path string, duration time.Duration, stderr io.Writer) (CoordinatorReadyPoolResponse, string, error) {
	var response CoordinatorReadyPoolResponse
	if err := validateReadyPoolSeedIdentity(identity, input); err != nil {
		return response, "", err
	}
	if duration <= 0 || duration > 30*time.Minute {
		return response, "", Exit(2, "borrow duration must be positive and at most 30m")
	}
	path, publicKey, err := createPoolAccessKey(path)
	if err != nil {
		return response, "", err
	}
	input = mapsCloneAny(input)
	input["identity"], input["publicKey"], input["durationSeconds"] = identity, publicKey, int64(duration/time.Second)
	if err = coord.poolAccessCall(ctx, key, "borrow", input, &response); err != nil {
		_ = removePoolAccessReceipt(path)
		return response, "", err
	}
	receipt := poolAccessReceipt{Schema: "crabbox-pool-access-receipt/v1", Pool: key, LeaseID: response.Entry.LeaseID,
		BorrowToken: response.BorrowToken, ReceiptToken: response.ReceiptToken, PublicKey: publicKey, Identity: identity}
	if response.Grant != nil {
		receipt.ExpiresAt = response.Grant.ExpiresAt
		receipt.GrantID, receipt.Generation = response.Grant.ID, response.Grant.Generation
	}
	if err = writePoolAccessReceipt(path, receipt); err != nil {
		return response, path, fmt.Errorf("save pending access receipt: %w", err)
	}
	fmt.Fprintf(stderr, "pending pool=%s lease=%s hard_deadline=%s receipt=%s\n", key, receipt.LeaseID, receipt.ExpiresAt, path)
	if response.Grant == nil || response.Grant.State != "pending" || response.Grant.LeaseID != receipt.LeaseID || receipt.BorrowToken == "" || receipt.ReceiptToken == "" || receipt.GrantID == "" || receipt.Generation < 1 || response.Grant.Fingerprint != fmt.Sprintf("%x", sha256.Sum256([]byte(publicKey))) {
		return response, path, Exit(4, "coordinator did not return a pending bounded grant")
	}
	if err = validateTypedReadyPoolResponseIdentity(response, identity); err == nil {
		err = readyPoolIdentityMatchesLease(identity, response.Lease)
	}
	if err != nil {
		return response, path, err
	}
	deadline, err := time.Parse(time.RFC3339Nano, receipt.ExpiresAt)
	if err != nil || !deadline.After(time.Now()) || deadline.After(time.Now().Add(duration)) {
		return response, path, Exit(4, "invalid borrow hard deadline")
	}
	expectedGrant := *response.Grant
	if err = coord.poolAccessCall(ctx, key, "ack", map[string]any{"leaseID": receipt.LeaseID, "borrowToken": receipt.BorrowToken, "receiptToken": receipt.ReceiptToken, "publicKey": publicKey}, &response); err != nil {
		return response, path, err
	}
	if response.Grant == nil || response.Grant.State != "active" || response.Grant.ID != expectedGrant.ID || response.Grant.ExpiresAt != receipt.ExpiresAt || response.Grant.Generation != expectedGrant.Generation {
		return response, path, Exit(4, "coordinator did not activate the acknowledged bounded grant")
	}
	response.BorrowToken, response.ReceiptToken, response.Entry.BorrowToken = "", "", ""
	fmt.Fprintf(stderr, "active pool=%s lease=%s hard_deadline=%s receipt=%s\n", key, receipt.LeaseID, receipt.ExpiresAt, path)
	return response, path, nil
}

type poolAccessTimings struct {
	FirstCommandMs int64 `json:"firstCommandMs"`
	ScrubMs        int64 `json:"scrubMs"`
	ScrubFailed    bool  `json:"scrubFailed"`
}

func returnPortablePool(ctx context.Context, coord *CoordinatorClient, path, result string, timing ...poolAccessTimings) error {
	receipt, err := readPoolAccessReceipt(path)
	if err != nil {
		return err
	}
	for {
		var response CoordinatorReadyPoolResponse
		input := map[string]any{"leaseID": receipt.LeaseID, "borrowToken": receipt.BorrowToken, "receiptToken": receipt.ReceiptToken, "result": result, "identity": receipt.Identity}
		if len(timing) > 0 {
			input["timing"] = timing[0]
		}
		err := coord.poolAccessCall(ctx, receipt.Pool, "return", input, &response)
		if err != nil {
			return err
		}
		if response.Grant == nil || response.Grant.ID != receipt.GrantID || response.Grant.Generation != receipt.Generation || response.Grant.LeaseID != receipt.LeaseID || response.Grant.ExpiresAt != receipt.ExpiresAt {
			return Exit(4, "coordinator returned a different access grant; retain the receipt")
		}
		if response.Grant.State == "revoked" {
			return removePoolAccessReceipt(path)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pool fencing pending; retain receipt %s: %w", path, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func heartbeatPortablePool(ctx context.Context, coord *CoordinatorClient, path string) error {
	receipt, err := readPoolAccessReceipt(path)
	if err != nil {
		return err
	}
	var response CoordinatorReadyPoolResponse
	err = coord.poolAccessCall(ctx, receipt.Pool, "heartbeat", map[string]any{"leaseID": receipt.LeaseID, "borrowToken": receipt.BorrowToken, "receiptToken": receipt.ReceiptToken}, &response)
	if err == nil && (response.Grant == nil || response.Grant.ID != receipt.GrantID || response.Grant.Generation != receipt.Generation || response.Grant.ExpiresAt != receipt.ExpiresAt) {
		return Exit(4, "coordinator changed immutable borrow expiry")
	}
	return err
}

func startPortablePoolHeartbeat(ctx context.Context, coord *CoordinatorClient, path string, stderr io.Writer) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(readyPoolBorrowHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				callCtx, stop := context.WithTimeout(ctx, 20*time.Second)
				err := heartbeatPortablePool(callCtx, coord, path)
				stop()
				if err != nil {
					fmt.Fprintf(stderr, "warning: portable pool heartbeat failed; access deadline is unchanged: %v\n", err)
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}
