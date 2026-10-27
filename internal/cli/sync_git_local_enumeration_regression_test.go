package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestLocalGitSeedLargeEnumerationHelper(t *testing.T) {
	oid := os.Getenv("CRABBOX_TEST_ENUMERATION_OID")
	if oid == "" {
		return
	}
	count, err := strconv.Atoi(os.Getenv("CRABBOX_TEST_ENUMERATION_COUNT"))
	if err != nil {
		os.Exit(2)
	}
	chunk := strings.Repeat(oid+"\n", 256)
	for count > 0 {
		n := min(count, 256)
		if _, err := os.Stdout.WriteString(chunk[:n*(len(oid)+1)]); err != nil {
			os.Exit(3)
		}
		count -= n
	}
	os.Exit(0)
}

func TestLocalGitSeedArtifactLargeEnumeration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git output wrapper; parser and process tests also run on Windows")
	}
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			root := localSeedTestRepo(t, format)
			head, tree, blob := localSeedTestCommit(t, root, "large enumeration fixture")
			localSeedTestGit(t, root, "", "update-ref", "refs/heads/main", head)
			for _, tc := range []struct {
				name  string
				count int
				want  string
			}{
				{"beyond metadata capture", localGitSeedMaxMetadata/(len(blob)+1) + 1, ""},
				{"over record and wire bound", localGitSeedMaxObjects + 1, "limit exceeded"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					git := gitOverlayGitExecutable
					wrapper := filepath.Join(t.TempDir(), "git")
					// Keep real Git's closure and append duplicate valid records. This
					// exercises full bundle preparation with a bounded, cheap fixture.
					script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in\n*' rev-list '*)\n%s \"$@\" || exit\nCRABBOX_TEST_ENUMERATION_OID=%s CRABBOX_TEST_ENUMERATION_COUNT=%d exec %s -test.run=^TestLocalGitSeedLargeEnumerationHelper$\n;;\n*) exec %s \"$@\" ;;\nesac\n",
						shellQuote(git), shellQuote(blob), tc.count, shellQuote(os.Args[0]), shellQuote(git))
					writeExecutable(t, wrapper, script)
					gitOverlayGitExecutable = wrapper
					t.Cleanup(func() { gitOverlayGitExecutable = git })
					artifact, err := prepareLocalGitSeedArtifact(context.Background(), root, localGitSeedSelection{Head: head, Tree: tree}, 0)
					if tc.want != "" {
						if err == nil {
							_ = artifact.cleanup()
							t.Fatal("over-limit enumeration succeeded")
						}
						if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "unshallow") {
							t.Fatalf("original refusal lost: %v", err)
						}
						if artifact.Root != "" {
							t.Fatalf("refused artifact retained: %s", artifact.Root)
						}
						return
					}
					if err != nil {
						t.Fatalf("enumeration beyond 16 MiB failed: %v", err)
					}
					t.Cleanup(func() {
						if err := artifact.cleanup(); err != nil {
							t.Error(err)
						}
					})
					if artifact.ObjectCount != 3 {
						t.Fatalf("duplicates changed closure: %d objects", artifact.ObjectCount)
					}
					localSeedTestImport(t, artifact)
				})
			}
		})
	}
}
