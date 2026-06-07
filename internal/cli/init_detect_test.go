package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectInitProjectPackageManagers(t *testing.T) {
	dir := t.TempDir()
	writeInitDetectFile(t, dir, "app/package.json", `{"packageManager":"pnpm@10.0.0","scripts":{"test:ci":"pnpm test -- --ci"}}`)
	writeInitDetectFile(t, dir, "app/pnpm-lock.yaml", "lockfileVersion: '9.0'\n")
	writeInitDetectFile(t, dir, "tool/package.json", `{"packageManager":"bun@1.2.0","scripts":{"build":"bun build"}}`)
	writeInitDetectFile(t, dir, "tool/bun.lockb", "")
	writeInitDetectFile(t, dir, "web/package.json", `{"packageManager":"yarn@4.0.0","scripts":{"test":"vitest run"}}`)
	writeInitDetectFile(t, dir, "web/yarn.lock", "")
	writeInitDetectFile(t, dir, "rust/Cargo.toml", "[package]\nname = \"demo\"\nversion = \"0.1.0\"\n")
	writeInitDetectFile(t, dir, "Makefile", ".PHONY: test\ntest:\n\tgo test ./...\n")

	got := detectInitProject(dir)
	for _, want := range []string{
		"(cd 'app' && corepack enable && pnpm install --frozen-lockfile && pnpm run 'test:ci')",
		"(cd 'tool' && bun install --frozen-lockfile && bun run 'build')",
		"(cd 'web' && corepack enable && yarn install --immutable && yarn 'test')",
		"(cd 'rust' && cargo test)",
		"make test",
	} {
		if !initDetectContains(got.Commands, want) {
			t.Fatalf("detected commands missing %q: %#v", want, got.Commands)
		}
	}
	for _, want := range []string{"corepack", "pnpm", "bun", "yarn", "cargo", "make"} {
		if !initDetectContains(got.PreflightTools, want) {
			t.Fatalf("detected tools missing %q: %#v", want, got.PreflightTools)
		}
	}
	if err := validatePreflightTools(got.PreflightTools); err != nil {
		t.Fatalf("detected preflight tools should validate: %v", err)
	}
}

func TestDetectInitProjectSkipsWorkspacePackagesWithoutLockfile(t *testing.T) {
	dir := t.TempDir()
	writeInitDetectFile(t, dir, "package.json", `{"scripts":{"test":"node --test"},"workspaces":["packages/*"]}`)
	writeInitDetectFile(t, dir, "packages/a/package.json", `{"scripts":{"test":"node --test"}}`)
	writeInitDetectFile(t, dir, "packages/b/package.json", `{"scripts":{"test":"node --test"}}`)
	writeInitDetectFile(t, dir, "packages/b/package-lock.json", `{"lockfileVersion": 3}`)

	got := detectInitProject(dir)
	for _, want := range []string{
		"npm install && npm test",
		"(cd 'packages/b' && npm ci && npm test)",
	} {
		if !initDetectContains(got.Commands, want) {
			t.Fatalf("detected commands missing %q: %#v", want, got.Commands)
		}
	}
	for _, unwanted := range []string{"packages/a"} {
		for _, command := range got.Commands {
			if strings.Contains(command, unwanted) {
				t.Fatalf("unexpected command %q in %#v", unwanted, got.Commands)
			}
		}
	}
}

func TestDetectInitProjectSkipsPlaceholderNpmTest(t *testing.T) {
	dir := t.TempDir()
	writeInitDetectFile(t, dir, "package.json", `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`)

	got := detectInitProject(dir)
	if len(got.Commands) != 0 {
		t.Fatalf("placeholder npm test should not produce commands: %#v", got.Commands)
	}
}

func writeInitDetectFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func initDetectContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
