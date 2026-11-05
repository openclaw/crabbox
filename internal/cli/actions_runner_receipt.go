package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// A portable receipt identifies only the exact registration to remove. It
// grants no lease access; cleanup still requires current GitHub credentials.
type actionsRunnerReceipt struct {
	Version  int    `json:"version"`
	LeaseID  string `json:"leaseID"`
	Scope    string `json:"scope"`
	RunnerID int64  `json:"runnerID"`
	Name     string `json:"name"`
}

func writeActionsRunnerReceipt(path, leaseID string, binding ActionsRunnerBinding) error {
	if path == "" {
		return nil
	}
	receipt := actionsRunnerReceipt{Version: 1, LeaseID: leaseID, Scope: binding.Scope, RunnerID: binding.RunnerID, Name: binding.Name}
	if err := receipt.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(receipt)
	if err == nil {
		err = writeStateFileAtomic(path, append(data, '\n'), syncControllerDirectory)
	}
	if err != nil {
		return Exit(7, "write cleanup receipt for %s/%d: %v", binding.Scope, binding.RunnerID, err)
	}
	return nil
}

func readActionsRunnerReceipt(path string) (actionsRunnerReceipt, error) {
	var receipt actionsRunnerReceipt
	file, err := os.Open(path)
	if err != nil {
		return receipt, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil {
		return receipt, err
	}
	if len(data) > 8192 {
		return receipt, Exit(2, "runner cleanup receipt exceeds 8192 bytes")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, fmt.Errorf("invalid runner cleanup receipt: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return receipt, Exit(2, "runner cleanup receipt must be one JSON object")
	}
	return receipt, receipt.validate()
}

func (r actionsRunnerReceipt) validate() error {
	if r.Version != 1 || !IsCanonicalLeaseID(r.LeaseID) || !actionsRunnerScopePattern.MatchString(r.Scope) || r.RunnerID <= 0 || r.Name == "" {
		return Exit(2, "invalid runner cleanup receipt identity or version")
	}
	return nil
}
