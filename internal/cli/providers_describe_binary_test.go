package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProvidersDescribeBuiltBinaryContract(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	binary := filepath.Join(t.TempDir(), "crabbox")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/crabbox")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/crabbox: %v\n%s", err, output)
	}

	isolation := t.TempDir()
	home := filepath.Join(isolation, "home")
	configHome := filepath.Join(isolation, "config")
	stateHome := filepath.Join(isolation, "state")
	cacheHome := filepath.Join(isolation, "cache")
	tmp := filepath.Join(isolation, "tmp")
	for _, dir := range []string{home, configHome, stateHome, cacheHome, tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	invalidConfig := filepath.Join(isolation, "invalid-config.yaml")
	if err := os.WriteFile(invalidConfig, []byte("[CONFIG_FILE_SECRET_MARKER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + configHome,
		"XDG_STATE_HOME=" + stateHome,
		"XDG_CACHE_HOME=" + cacheHome,
		"TMPDIR=" + tmp,
		"CRABBOX_CONFIG=" + invalidConfig,
		"CRABBOX_LOCAL_CONTAINER_IMAGE=ENV_SECRET_MARKER",
		"CRABBOX_SERVER_TYPE=SERVER_TYPE_SECRET_MARKER",
		"CRABBOX_COORDINATOR_TOKEN=TOKEN_SECRET_MARKER",
		"HTTPS_PROXY=http://127.0.0.1:1",
		"HTTP_PROXY=http://127.0.0.1:1",
	}
	before, err := snapshotTestTree(isolation)
	if err != nil {
		t.Fatal(err)
	}

	matrixOut, matrixErr, code := runDescribeTestBinary(binary, root, env, "providers", "--json")
	if code != 0 {
		t.Fatalf("providers --json exit=%d stderr=%s", code, matrixErr)
	}
	var entries []providerMatrixEntry
	if err := json.Unmarshal(matrixOut, &entries); err != nil {
		t.Fatalf("providers --json: %v\n%s", err, matrixOut)
	}
	for _, entry := range entries {
		stdout, stderr, exitCode := runDescribeTestBinary(binary, root, env, "providers", "describe", entry.Provider, "--json")
		switch entry.Kind {
		case ProviderKindSSHLease, ProviderKindDelegatedRun:
			if exitCode != 0 {
				t.Errorf("describe %s exit=%d stderr=%s", entry.Provider, exitCode, stderr)
				continue
			}
			var description providerDescription
			if err := json.Unmarshal(stdout, &description); err != nil {
				t.Errorf("describe %s JSON: %v\n%s", entry.Provider, err, stdout)
				continue
			}
			if !description.Runnable || description.Provider.Canonical != entry.Provider {
				t.Errorf("describe %s identity=%#v runnable=%t", entry.Provider, description.Provider, description.Runnable)
			}
		case ProviderKindServiceControl:
			if exitCode != 2 || len(stdout) != 0 || !strings.Contains(string(stderr), "not runnable") {
				t.Errorf("service-control describe %s exit=%d stdout=%q stderr=%q", entry.Provider, exitCode, stdout, stderr)
			}
		default:
			if exitCode != 2 || len(stdout) != 0 || !strings.Contains(string(stderr), "unsupported kind") {
				t.Errorf("unknown-kind describe %s exit=%d stdout=%q stderr=%q", entry.Provider, exitCode, stdout, stderr)
			}
		}
	}

	stdout, stderr, exitCode := runDescribeTestBinary(binary, root, env, "providers", "describe", "missing-provider", "--json")
	if exitCode != 2 || len(stdout) != 0 || !strings.Contains(string(stderr), "unknown provider") {
		t.Fatalf("unknown describe exit=%d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}

	aliasOut, aliasErr, aliasCode := runDescribeTestBinary(binary, root, env, "providers", "describe", "--json", "docker")
	if aliasCode != 0 {
		t.Fatalf("describe docker exit=%d stderr=%s", aliasCode, aliasErr)
	}
	if bytes.Contains(aliasOut, []byte("SECRET_MARKER")) {
		t.Fatalf("describe output leaked config/env marker: %s", aliasOut)
	}
	var local providerDescription
	if err := json.Unmarshal(aliasOut, &local); err != nil {
		t.Fatal(err)
	}
	if local.Provider.Requested != "docker" || local.Provider.Canonical != "local-container" || local.Provider.InputAlias != "docker" || local.Family != "container" {
		t.Fatalf("local-container identity=%#v family=%q", local.Provider, local.Family)
	}
	localFlags := descriptionFlagMap(local.ProviderFlags)
	wantLocal := []string{
		"local-container-cpus", "local-container-docker-socket", "local-container-image",
		"local-container-memory", "local-container-network", "local-container-runtime",
		"local-container-user", "local-container-volume", "local-container-work-root",
	}
	if got := sortedDescriptionFlagNames(local.ProviderFlags); !reflect.DeepEqual(got, wantLocal) {
		t.Fatalf("local-container flags=%v want %v", got, wantLocal)
	}
	if defaults, ok := localFlags["local-container-volume"].Default.([]any); !ok || defaults == nil || len(defaults) != 0 {
		t.Fatalf("local-container volume default=%#v", localFlags["local-container-volume"].Default)
	}
	sharedFlags := descriptionFlagMap(local.SharedFlags)
	if sharedFlags["type"].Default != baseConfig().ServerType {
		t.Fatalf("type default=%#v, want compiled base default %#v", sharedFlags["type"].Default, baseConfig().ServerType)
	}
	for _, excluded := range []string{"aws-lambda-microvm-image", "azure-backend", "daytona-api-url", "tart-image"} {
		if _, ok := localFlags[excluded]; ok {
			t.Errorf("local-container output included --%s", excluded)
		}
	}

	appleOut, appleErr, appleCode := runDescribeTestBinary(binary, root, env, "providers", "describe", "apple-vm", "--json")
	if appleCode != 0 {
		t.Fatalf("describe apple-vm exit=%d stderr=%s", appleCode, appleErr)
	}
	var apple providerDescription
	if err := json.Unmarshal(appleOut, &apple); err != nil {
		t.Fatal(err)
	}
	for _, item := range apple.ProviderFlags {
		if strings.HasPrefix(item.Name, "apple-vz-") && (!item.Deprecated || !strings.HasPrefix(item.Replacement, "apple-vm-")) {
			t.Errorf("Apple VM deprecated metadata=%#v", item)
		}
	}

	after, err := snapshotTestTree(isolation)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("providers describe mutated isolated filesystem\nbefore=%v\nafter=%v", before, after)
	}

	helpEnv := []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + configHome, "XDG_STATE_HOME=" + stateHome,
		"XDG_CACHE_HOME=" + cacheHome, "TMPDIR=" + tmp,
	}
	helpStdout, helpStderr, helpCode := runDescribeTestBinary(binary, root, helpEnv, "run", "--help")
	if helpCode != 0 || len(helpStdout) != 0 {
		t.Fatalf("run --help exit=%d stdout=%q stderr=%q", helpCode, helpStdout, helpStderr)
	}
	digest := sha256.Sum256(helpStderr)
	const baselineSHA256 = "dabee7181e3b5d02252ec5c922d3d02cadff961a4232ba84b73988436c7a3a5f"
	if got := hex.EncodeToString(digest[:]); got != baselineSHA256 || len(helpStderr) != 61216 {
		t.Fatalf("run --help changed: sha256=%s bytes=%d, want sha256=%s bytes=61216", got, len(helpStderr), baselineSHA256)
	}
}

func runDescribeTestBinary(binary, dir string, env []string, args ...string) (stdout, stderr []byte, exitCode int) {
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err == nil {
		return outBuf.Bytes(), errBuf.Bytes(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return outBuf.Bytes(), errBuf.Bytes(), exitErr.ExitCode()
	}
	return outBuf.Bytes(), append(errBuf.Bytes(), []byte(fmt.Sprintf("%v", err))...), -1
}

func snapshotTestTree(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := fmt.Sprintf("%s:%d", info.Mode(), info.Size())
		if !entry.IsDir() {
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(contents)
			value += ":" + hex.EncodeToString(digest[:])
		}
		out[relative] = value
		return nil
	})
	return out, err
}

func sortedDescriptionFlagNames(flags []providerDescriptionFlag) []string {
	names := make([]string, 0, len(flags))
	for _, item := range flags {
		names = append(names, item.Name)
	}
	return names
}
