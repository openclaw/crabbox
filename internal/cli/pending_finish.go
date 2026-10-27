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
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Body is base64-encoded by encoding/json so the original HTTP bytes survive
// envelope encoding, storage and import unchanged. Authentication headers and
// signing keys are never included; retained task output may still be sensitive.
type pendingRunFinish struct {
	SchemaVersion int    `json:"schema_version"`
	Coordinator   string `json:"coordinator"`
	RunID         string `json:"run_id"`
	Owner         string `json:"owner"`
	Org           string `json:"org"`
	Body          []byte `json:"body"`
	BodySHA256    string `json:"body_sha256"`
}

// JSON escaping and the outer base64 can expand the retained log. Keep imports
// bounded without truncating, normalizing or rebuilding signed evidence.
const maxPendingRunFinishBytes = 16*maxRunLogBytes + 1<<20

type pendingFinishBody struct {
	ExitCode     int             `json:"exitCode"`
	SyncMs       int64           `json:"syncMs"`
	CommandMs    int64           `json:"commandMs"`
	Log          string          `json:"log"`
	LogChunks    []string        `json:"logChunks"`
	LogTruncated bool            `json:"logTruncated"`
	Results      json.RawMessage `json:"results"`
	Telemetry    json.RawMessage `json:"telemetry,omitempty"`
	BlockedStage string          `json:"blockedStage,omitempty"`
	RetryLikely  string          `json:"retryLikely,omitempty"`
	Receipt      json.RawMessage `json:"receipt"`
}

func decodePendingFinishJSON(data []byte, out any) error {
	if len(data) == 0 || len(data) > maxPendingRunFinishBytes || !utf8.Valid(data) {
		return fmt.Errorf("invalid pending finish size or encoding")
	}
	duplicate, err := JSONHasDuplicateKeys(json.NewDecoder(bytes.NewReader(data)))
	if err != nil {
		return err
	}
	if duplicate {
		return errDuplicateReceiptKey
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	return requireJSONEOF(dec)
}

func validatePendingRunFinish(p pendingRunFinish) (pendingFinishBody, terminalRunReceipt, error) {
	var body pendingFinishBody
	var receipt terminalRunReceipt
	u, err := url.Parse(p.Coordinator)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimRight(p.Coordinator, "/") != p.Coordinator {
		return body, receipt, fmt.Errorf("invalid original coordinator route")
	}
	if p.SchemaVersion != 1 || p.RunID == "" || p.BodySHA256 != sha256Digest(p.Body) {
		return body, receipt, fmt.Errorf("invalid pending finish identity or request digest")
	}
	if err := decodePendingFinishJSON(p.Body, &body); err != nil {
		return body, receipt, err
	}
	receipt, err = decodeTerminalRunReceipt(body.Receipt)
	if err != nil {
		return body, receipt, err
	}
	log := strings.Join(body.LogChunks, "")
	if len(log) > maxRunLogBytes || !utf8.ValidString(log) || body.Log != runLogFallbackPreview(log) {
		return body, receipt, fmt.Errorf("pending finish retained log is inconsistent")
	}
	if receipt.RunID != p.RunID || receipt.ExitCode != body.ExitCode || receipt.SyncMs != body.SyncMs || receipt.CommandMs != body.CommandMs || receipt.LogTruncated != body.LogTruncated || receipt.RetainedLogSHA256 != sha256Digest([]byte(log)) || (!body.LogTruncated && receipt.LogSHA256 != receipt.RetainedLogSHA256) {
		return body, receipt, fmt.Errorf("pending finish signed payload binding mismatch")
	}
	return body, receipt, nil
}

func pendingRunFinishPath(p pendingRunFinish) (string, error) {
	state, err := CrabboxStateDir()
	if err != nil {
		return "", err
	}
	// Neither the run identifier nor the coordinator can introduce a filesystem path.
	name := strings.TrimPrefix(sha256Digest([]byte(p.Coordinator+"\x00"+p.RunID)), "sha256:") + ".json"
	return filepath.Join(state, "pending-finishes", name), nil
}

func retainPendingRunFinish(p pendingRunFinish) (string, error) {
	if _, _, err := validatePendingRunFinish(p); err != nil {
		return "", err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if len(data) > maxPendingRunFinishBytes {
		return "", fmt.Errorf("pending finish exceeds retention bound")
	}
	path, err := pendingRunFinishPath(p)
	if err != nil {
		return "", err
	}
	root, err := crabboxStateRootDir()
	if err != nil {
		return "", err
	}
	if err := ensurePrivateDirectoryDurableWithSync(filepath.Dir(path), root, syncControllerDirectory); err != nil {
		return "", err
	}
	file, temp, err := createPrivateRunOutputTemp(path)
	if err != nil {
		return "", err
	}
	defer os.Remove(temp)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := publishPendingFinish(temp, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		retained, err := readPendingFinishFile(path)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(retained, data) {
			return "", fmt.Errorf("conflicting pending finish for %s; preserve the original", p.RunID)
		}
	}
	if err := os.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := syncControllerDirectory(filepath.Dir(path)); err != nil {
		return "", err
	}
	return path, nil
}

func removePendingRunFinish(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncControllerDirectory(filepath.Dir(path))
}

func recoverPendingRunFinish(ctx context.Context, coord *CoordinatorClient, runID, path, signer, digest string) (terminalRunReceipt, error) {
	var empty terminalRunReceipt
	if !validSHA256Digest(signer) || !validSHA256Digest(digest) {
		return empty, Exit(2, "recovery requires --expected-signer and --expected-finish-sha256 from the original trusted finish")
	}
	data, err := readPendingFinishFile(path)
	if err != nil {
		return empty, err
	}
	var pending pendingRunFinish
	if err := decodePendingFinishJSON(data, &pending); err != nil {
		return empty, Exit(2, "decode pending finish: %v", err)
	}
	body, receipt, err := validatePendingRunFinish(pending)
	if err != nil {
		return empty, Exit(2, "invalid original finish: %v", err)
	}
	if pending.Owner == "" || pending.Org == "" || pending.RunID != runID || receipt.Signer != signer || pending.BodySHA256 != digest || pending.Coordinator != strings.TrimRight(coord.BaseURL, "/") {
		return empty, Exit(2, "original finish run, signer, digest or coordinator mismatch")
	}
	ctx, cancel := context.WithTimeout(ctx, runRecorderFinishTimeout)
	defer cancel()
	// Capture once, in memory only: token-command identity must not change between
	// Whoami, ownership verification, POST and readback. Never persist credentials.
	headers := make(http.Header)
	if err := coord.addRequestHeaders(ctx, headers); err != nil {
		return empty, err
	}
	bound := CoordinatorClient{
		BaseURL: strings.TrimRight(coord.BaseURL, "/"), Client: coord.Client,
		readRetryWriter: coord.readRetryWriter,
		admissionAuth:   &coordinatorAdmissionAuth{captured: true, headers: headers},
	}
	who, err := bound.Whoami(ctx)
	if err != nil {
		return empty, err
	}
	if who.Owner != pending.Owner || who.Org != pending.Org {
		return empty, Exit(2, "original finish owner or organization mismatch")
	}
	run, err := bound.Run(ctx, runID)
	if err != nil {
		return empty, err
	}
	if run.Owner != pending.Owner || run.Org != pending.Org {
		return empty, Exit(2, "run owner or organization mismatch")
	}
	started, err := time.Parse(time.RFC3339Nano, run.StartedAt)
	if err != nil {
		return empty, err
	}
	if err := verifyTerminalRunReceipt(receipt, terminalRunReceiptInput{
		Provider: run.Provider, LeaseID: run.LeaseID, Slug: run.Slug, RunID: run.ID, Command: run.Command,
		ExitCode: body.ExitCode, SyncMs: body.SyncMs, CommandMs: body.CommandMs,
		StartedAt: started, EndedAt: time.Now(), LogTruncated: body.LogTruncated,
		RetainedLogSHA256: sha256Digest([]byte(strings.Join(body.LogChunks, ""))),
	}); err != nil {
		return empty, Exit(2, "original finish does not bind the recorded run: %v", err)
	}
	// One explicit idempotent submission. A conflict is never converted to success
	// merely because another finish happens to contain the same receipt.
	if _, err := bound.finishRunBytes(ctx, runID, pending.Body); err != nil {
		return empty, Exit(7, "original finish submission not confirmed; retained %s: %v", path, err)
	}
	committed, err := readVerifiedTerminalReceipt(ctx, &bound, runID, signer)
	if err != nil {
		return empty, err
	}
	if committed != receipt {
		return empty, Exit(7, "stored terminal receipt differs from the original finish")
	}
	// Import is non-destructive: keep the caller's original file as evidence.
	return committed, nil
}

// Read through an owner/private/no-follow handle, never chmod an imported file.
func readPendingFinishFile(path string) ([]byte, error) {
	file, err := openPendingFinishFile(path)
	if err != nil {
		return nil, fmt.Errorf("open private pending finish: %w", err)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > maxPendingRunFinishBytes {
		return nil, fmt.Errorf("pending finish must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxPendingRunFinishBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if len(data) > maxPendingRunFinishBytes || !adapterIngressPrivateFileStable(before, after, int64(len(data))) {
		return nil, fmt.Errorf("pending finish changed while reading or exceeded size bound")
	}
	return data, nil
}
