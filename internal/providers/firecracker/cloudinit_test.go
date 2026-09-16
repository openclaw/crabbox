package firecracker

import (
	"crypto/sha256"
	"fmt"
	"testing"

	shared "github.com/openclaw/crabbox/internal/providers/shared"
)

func TestBuildFAT16ImagePreservesFirecrackerEncoding(t *testing.T) {
	image, err := buildFAT16Image("cidata", []shared.FATFile{
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
