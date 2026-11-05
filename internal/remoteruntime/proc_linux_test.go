package remoteruntime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGroupHasLiveMembers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stats map[string]string
		live  bool
		err   bool
	}{
		{name: "guard only", stats: map[string]string{"10": "10 (guard) S 1 10"}},
		{name: "live member", stats: map[string]string{"10": "10 (guard) S 1 10", "20": "20 (a ) ( b) S 1 10"}, live: true},
		{name: "zombie and other group", stats: map[string]string{"10": "10 (guard) S 1 10", "20": "20 (child) Z 1 10", "30": "30 (other) S 1 30"}},
		{name: "missing guard", err: true},
		{name: "unreadable member", stats: map[string]string{"10": "10 (guard) S 1 10", "20": "directory"}, err: true},
		{name: "malformed member", stats: map[string]string{"10": "10 (guard) S 1 10", "20": "20 (child) S"}, err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for pid, stat := range tc.stats {
				dir := filepath.Join(root, pid)
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if stat == "directory" {
					if err := os.Mkdir(filepath.Join(dir, "stat"), 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0600); err != nil {
					t.Fatal(err)
				}
			}
			live, err := groupHasLiveMembers(root, 10)
			if live != tc.live || (err != nil) != tc.err {
				t.Fatalf("live=%t, err=%v; want live=%t, error=%t", live, err, tc.live, tc.err)
			}
		})
	}
}
