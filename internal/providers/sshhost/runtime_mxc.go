package sshhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

type mxcPhaseError struct{ phase, code string }

func (e *mxcPhaseError) Error() string {
	return fmt.Sprintf("MXC %s failed (%s); custody retained", e.phase, e.code)
}

// Failure envelopes are meaningful even when the executor exits nonzero. Never
// infer absence from an untyped process error or reproduce executor diagnostics.
func decodeMXCReceipt(phase string, data []byte, runErr error) (json.RawMessage, error) {
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &out) != nil {
		return nil, fmt.Errorf("MXC %s returned an invalid receipt", phase)
	}
	if out.Error != nil {
		return nil, &mxcPhaseError{phase: phase, code: out.Error.Code}
	}
	if runErr != nil || len(out.Result) == 0 || bytes.Equal(bytes.TrimSpace(out.Result), []byte("null")) {
		return nil, fmt.Errorf("MXC %s completion unconfirmed; custody retained", phase)
	}
	return out.Result, nil
}

func mxcDeprovisionConfirmedAbsent(err error) bool {
	var receipt *mxcPhaseError
	return errors.As(err, &receipt) && receipt.phase == "deprovision" && receipt.code == "stale_id"
}
