package firecracker

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/fat16"
)

func TestBuildFAT16ImagePreservesFirecrackerEncoding(t *testing.T) {
	image, err := buildFAT16Image("cidata", []fat16.File{
		{Name: "user-data", Data: []byte("#cloud-config\nusers:\n- name: alice\n")},
		{Name: "meta-data", Data: []byte("instance-id: crabbox-test\nlocal-hostname: my-app\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprintf("%x", sha256.Sum256(image)), "8cf6c03a8acdb3642216daadda7a475df2f93d14a90173cdb97a263b121af03d"; got != want {
		t.Fatalf("sha256=%s, want %s", got, want)
	}
}

func TestBuildFAT16ImagePreservesFirecrackerErrors(t *testing.T) {
	const maxDataBytes = ((20480 - (1 + 2*40 + 512*32/512)) / 4) * 4 * 512
	oversized := bytes.Repeat([]byte{'x'}, maxDataBytes+1)
	files256 := make([]fat16.File, 256)
	for i := range files256 {
		files256[i] = fat16.File{Name: fmt.Sprintf("f%03d", i)}
	}
	cases := []struct {
		name, suffix string
		files        []fat16.File
	}{
		{"blank-name-order", "file name is required", []fat16.File{{Name: " ", Data: oversized}}},
		{"capacity-plus-one", "payload is too large", []fat16.File{{Name: "payload.bin", Data: oversized}}},
		{"directory-256-files", "directory is too large", files256},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			image, err := buildFAT16Image("cidata", test.files)
			var exitErr core.ExitError
			if image != nil || !core.AsExitError(err, &exitErr) || exitErr.Code != 2 || exitErr.Message != "firecracker cloud-init "+test.suffix {
				t.Fatalf("imageNil=%v err=%#v", image == nil, err)
			}
		})
	}
}
