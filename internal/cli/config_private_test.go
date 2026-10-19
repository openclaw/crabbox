package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteUserFileConfigUsesPrivateStaging(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "create"
		if existing {
			name = "replace"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			prepareConfigPrivateParent(t, dir)
			path := filepath.Join(dir, "config.yaml")
			if existing {
				if err := os.WriteFile(path, []byte("previous fixture"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var staged string
			synced := false
			err := writeUserFileConfigAtomic(path, []byte("broker:\n  token: harmless-fixture\n"), func(from, to string) error {
				staged = from
				if to != path || filepath.Dir(from) != dir {
					t.Fatalf("unexpected publication paths %s -> %s", from, to)
				}
				assertConfigPrivate(t, from)
				if existing {
					if data, err := os.ReadFile(path); err != nil || string(data) != "previous fixture" {
						t.Fatalf("config changed before publication: %q, %v", data, err)
					}
				}
				return replaceClaimFile(from, to)
			}, func(path string) {
				if path != dir {
					t.Fatalf("sync directory=%s, want %s", path, dir)
				}
				synced = true
			})
			if err != nil {
				t.Fatal(err)
			}
			if !synced || staged == "" {
				t.Fatal("config did not complete staged publication and directory sync")
			}
			assertConfigPrivate(t, path)
			if _, err := os.Stat(staged); !os.IsNotExist(err) {
				t.Fatalf("staged file remains: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "broker:\n  token: harmless-fixture\n" {
				t.Fatalf("published config=%q error=%v", data, err)
			}
		})
	}
}
