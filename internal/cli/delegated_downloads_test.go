package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeDelegatedRunDownloadBackend struct {
	files   map[string][]byte
	fetches map[string]int
}

func (b *fakeDelegatedRunDownloadBackend) Spec() ProviderSpec {
	return ProviderSpec{Name: "test", Kind: ProviderKindDelegatedRun}
}

func (b *fakeDelegatedRunDownloadBackend) FetchRunFile(_ context.Context, req DelegatedRunDownloadRequest) ([]byte, error) {
	b.fetches[req.RemotePath]++
	data, ok := b.files[req.RemotePath]
	if !ok {
		return nil, os.ErrNotExist
	}
	return data, nil
}

func TestMaterializeDelegatedRunDownloadsCachesRequiredDownload(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "proof", "manifest.json")
	backend := &fakeDelegatedRunDownloadBackend{
		files: map[string][]byte{
			"reports/manifest.json": {},
		},
		fetches: map[string]int{},
	}
	var stderr bytes.Buffer
	artifacts, err := MaterializeDelegatedRunDownloads(t.Context(), backend, RunRequest{
		RequiredArtifactGlobs: []string{"reports/manifest.json"},
		Downloads:             []string{"reports/manifest.json=" + local},
	}, "lease-1", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if backend.fetches["reports/manifest.json"] != 1 {
		t.Fatalf("fetches=%d, want one", backend.fetches["reports/manifest.json"])
	}
	if len(artifacts) != 1 || artifacts[0].Kind != "delegated-download" || artifacts[0].Bytes != 0 {
		t.Fatalf("artifacts=%#v", artifacts)
	}
	if data, err := os.ReadFile(local); err != nil || len(data) != 0 {
		t.Fatalf("download data=%q err=%v", data, err)
	}
	for _, want := range []string{
		"required artifact reports/manifest.json matched=1",
		"downloaded " + local + " bytes=0",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestValidateDelegatedRunFilePath(t *testing.T) {
	for input, want := range map[string]string{
		"reports/manifest.json":         "reports/manifest.json",
		"./reports/proof.txt":           "reports/proof.txt",
		"results/a+b@example.test.json": "results/a+b@example.test.json",
	} {
		got, err := normalizeDelegatedRunFilePath("--download", input)
		if err != nil {
			t.Fatalf("path=%q err=%v", input, err)
		}
		if got != want {
			t.Fatalf("path=%q normalized=%q want %q", input, got, want)
		}
	}
	for _, path := range []string{
		"",
		"/tmp/proof",
		"../secret",
		"reports/*.json",
		`reports\proof.json`,
		"https://example.test/proof",
		".git/config",
		"./.git/config",
		"reports/.git/config",
		".crabbox/private",
		"./.crabbox/private",
		"reports/.crabbox/private",
	} {
		if err := validateDelegatedRunFilePath("--download", path); err == nil {
			t.Fatalf("path=%q should be rejected", path)
		}
	}
}

var _ DelegatedRunDownloadBackend = (*fakeDelegatedRunDownloadBackend)(nil)
