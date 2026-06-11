//go:build !darwin || !arm64 || !cgo

package applevzhelper

import (
	"encoding/json"
	"fmt"
	"io"
)

func RunCLI(_ []string, _ io.Reader, stdout, stderr io.Writer) int {
	_ = json.NewEncoder(stdout).Encode(DoctorResponse{
		Status:  "error",
		Message: "apple-vz helper requires darwin/arm64",
		Details: map[string]string{
			"host": "unsupported",
		},
	})
	fmt.Fprintln(stderr, "apple-vz helper requires darwin/arm64")
	return 2
}
