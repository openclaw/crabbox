package sshhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	core "github.com/openclaw/crabbox/internal/cli"
)

// MXC wraps commandLine in cmd.exe /c. A quoted executable followed by a
// quoted argument loses its outer quotes there; an encoded PowerShell command
// preserves both literal paths across the CMD and PowerShell parsers.
func mxcEndpointCommand(executable, state string) string {
	return core.PowershellCommand("& " + core.PowerShellQuote(executable) + " host endpoint --endpoint-state " + core.PowerShellQuote(state) + "; exit $LASTEXITCODE")
}

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
