package remoteruntime

import (
	"errors"
	"fmt"
	"strings"
)

const CompletionLimit = 256

type Completion struct {
	State          string
	WorkerQuiesced bool
	ScratchRemoved bool
	StageRetired   bool
}

func validNonce(nonce string) bool {
	return len(nonce) == 32 && strings.Trim(nonce, "0123456789abcdef") == ""
}

// ParseCompletion accepts only a complete owner acknowledgement. A worker's
// exit status or output cannot substitute for confirmed cleanup.
func ParseCompletion(record []byte, nonce string) (Completion, error) {
	var empty Completion
	unconfirmed := errors.New("functional preflight cleanup unconfirmed")
	if !validNonce(nonce) || len(record) > CompletionLimit {
		return empty, unconfirmed
	}
	fields := strings.Split(string(record), "\n")
	if len(fields) != 7 || fields[0] != "CBX-PREFLIGHT-1" || fields[1] != nonce ||
		fields[3] != "worker-quiesced" || fields[4] != "scratch-removed" || fields[5] != "complete" || fields[6] != "" {
		return empty, unconfirmed
	}
	switch fields[2] {
	case "ready", "missing-python3", "venv-unavailable", "pip-unavailable", "worker-failed", "timed-out", "canceled":
		return Completion{State: fields[2], WorkerQuiesced: true, ScratchRemoved: true}, nil
	default:
		return empty, unconfirmed
	}
}

func completionRecord(nonce, state string) ([]byte, error) {
	record := []byte(fmt.Sprintf("CBX-PREFLIGHT-1\n%s\n%s\nworker-quiesced\nscratch-removed\ncomplete\n", nonce, state))
	if _, err := ParseCompletion(record, nonce); err != nil {
		return nil, err
	}
	return record, nil
}
