//go:build !windows

package sshhost

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupPrivateGroupRequiresExactEmptyIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, record string
		wantErr      bool
	}{
		{"already absent", "", false},
		{"owned leftover", "cbhfixture:x:1234:", false},
		{"foreign gid", "cbhfixture:x:9999:", true},
		{"foreign member", "cbhfixture:x:1234:someone", true},
		{"foreign name", "foreign:x:1234:", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			group := filepath.Join(dir, "group-record")
			if tc.record != "" {
				if err := os.WriteFile(group, []byte(tc.record), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for name, script := range map[string]string{
				"getent":   "#!/bin/sh\n[ -f \"$GROUP_RECORD\" ] || exit 2\n/bin/cat \"$GROUP_RECORD\"\n",
				"groupdel": "#!/bin/sh\n/bin/rm \"$GROUP_RECORD\"\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir)
			t.Setenv("GROUP_RECORD", group)
			err := cleanupPrivateGroup(t.Context(), &runtimeState{User: "cbhfixture", GID: "1234"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v", err)
			}
			_, statErr := os.Stat(group)
			if tc.wantErr && statErr != nil {
				t.Fatal("foreign group was removed")
			}
			if !tc.wantErr && !os.IsNotExist(statErr) {
				t.Fatal("owned group remains")
			}
		})
	}
}
