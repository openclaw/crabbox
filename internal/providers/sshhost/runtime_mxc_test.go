package sshhost

import (
	"errors"
	"testing"
)

func TestMXCDeprovisionReceipts(t *testing.T) {
	for _, tc := range []struct {
		name, phase, data string
		runErr            error
		ok, absent        bool
	}{
		{"success", "deprovision", `{"result":{},"error":null}`, nil, true, false},
		{"lost prior completion", "deprovision", `{"error":{"code":"stale_id"}}`, errors.New("exit 1"), false, true},
		{"failed stop is not absence", "stop", `{"error":{"code":"stale_id"}}`, errors.New("exit 1"), false, false},
		{"transient failure", "deprovision", `{"error":{"code":"backend_error"}}`, errors.New("exit 1"), false, false},
		{"missing receipt", "deprovision", `{}`, nil, false, false},
		{"null result", "deprovision", `{"result":null}`, nil, false, false},
		{"process failed", "deprovision", `{"result":{}}`, errors.New("exit 1"), false, false},
		{"unstructured failure", "deprovision", `stale_id`, errors.New("exit 1"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeMXCReceipt(tc.phase, []byte(tc.data), tc.runErr)
			if (err == nil) != tc.ok || mxcDeprovisionConfirmedAbsent(err) != tc.absent {
				t.Fatalf("err=%v absent=%v", err, mxcDeprovisionConfirmedAbsent(err))
			}
		})
	}
}
