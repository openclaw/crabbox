package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLocalActionsHydrateScriptPnpmBeforeNode(t *testing.T) {
	var workflow localHydrateWorkflow
	if err := yaml.Unmarshal([]byte(`env:
  PNPM_VERSION: 11.25.0
jobs:
  hydrate:
    steps:
      - uses: actions/checkout@v7.0.1
        with:
          ref: ${{ inputs.ref || github.ref }}
      - uses: pnpm/action-setup@v6.0.10
        with:
          version: ${{ env.PNPM_VERSION }}
      - uses: actions/setup-node@v7
        with:
          node-version: 24
          cache: pnpm
      - run: pnpm install --frozen-lockfile
`), &workflow); err != nil {
		t.Fatal(err)
	}
	got, err := localActionsHydrateScript(defaultConfig(), Repo{Name: "my-app"}, workflow, workflow.Jobs["hydrate"], "hydrate", "cbx_123", nil, "/work/cbx_123/repo")
	if err != nil {
		t.Fatal(err)
	}
	pnpm := strings.Index(got, "__crabbox_setup_pnpm '11.25.0'")
	node := strings.Index(got, "__crabbox_setup_node '24'")
	install := strings.Index(got, "pnpm install --frozen-lockfile")
	if pnpm < 0 || node < pnpm || install < node {
		t.Fatalf("expected pnpm setup, Node setup, then install: pnpm=%d node=%d install=%d", pnpm, node, install)
	}
	if !strings.Contains(got, "pnpm cache restore/save skipped") || strings.Contains(got, "${{") {
		t.Fatalf("expected explicit uncached translation and resolved inputs:\n%s", got)
	}
}

func TestLocalActionsHydrateScriptPnpmInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		with map[string]string
		want string
	}{
		{name: "missing version", want: "explicit exact version"},
		{name: "major", with: map[string]string{"version": "11"}, want: "explicit exact version"},
		{name: "range", with: map[string]string{"version": "^11.0.0"}, want: "explicit exact version"},
		{name: "tag", with: map[string]string{"version": "latest"}, want: "explicit exact version"},
		{name: "install", with: map[string]string{"version": "11.25.0", "run_install": "true"}, want: "option"},
		{name: "standalone", with: map[string]string{"version": "11.25.0", "standalone": "true"}, want: "option"},
		{name: "destination", with: map[string]string{"version": "11.25.0", "dest": "custom"}, want: "option"},
		{name: "manifest", with: map[string]string{"version": "11.25.0", "package_json_file": "other.json"}, want: "option"},
		{name: "cache", with: map[string]string{"version": "11.25.0", "cache": "true"}, want: "option"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := localHydrateUsesScript(localHydrateStep{Uses: "pnpm/action-setup@v6.0.10", With: tc.with}, localHydrateScriptContext{}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "--github-runner") {
				t.Fatalf("err=%v, want %q with GitHub runner guidance", err, tc.want)
			}
		})
	}
}

func TestLocalActionsHydrateScriptSetupNodeCache(t *testing.T) {
	for _, cache := range []string{"", "pnpm", "${{ env.CACHE }}", "npm", "yarn", "unknown"} {
		t.Run(cache, func(t *testing.T) {
			script, _, err := localHydrateUsesScript(localHydrateStep{Uses: "actions/setup-node@v7", With: map[string]string{"node-version": "24", "cache": cache}}, localHydrateScriptContext{}, map[string]string{"CACHE": "pnpm"})
			if cache == "npm" || cache == "yarn" || cache == "unknown" {
				if err == nil || !strings.Contains(err.Error(), "--github-runner") {
					t.Fatalf("expected unsupported cache error, got %v", err)
				}
				return
			}
			if err != nil || !strings.Contains(script, "__crabbox_setup_node '24'") {
				t.Fatalf("script=%q err=%v", script, err)
			}
		})
	}
}

func TestLocalActionsSetupPnpmBootstrapsNode(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// An allowlisted PATH proves setup does not depend on an operator's Node/npm.
	for _, name := range []string{"mkdir", "rm", "cat", "chmod"} {
		target, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	script := "set -euo pipefail\n" + localActionsRuntimeShell() + `
__crabbox_setup_node() {
  [ "$1" = 24 ]
  mkdir -p "$RUNNER_TOOL_CACHE/node/bin"
  cat >"$RUNNER_TOOL_CACHE/node/bin/node" <<'NODE'
#!/bin/sh
printf '24.0.0\n'
NODE
  cat >"$RUNNER_TOOL_CACHE/node/bin/npm" <<'NPM'
#!/bin/sh
printf '%s\n' "$@" >"$NPM_ARGS"
prefix=
while [ "$#" -gt 0 ]; do
  if [ "$1" = --prefix ]; then shift; prefix="$1"; fi
  shift
done
mkdir -p "$prefix/bin"
printf '#!/bin/sh\nprintf "11.25.0\\n"\n' >"$prefix/bin/pnpm"
chmod +x "$prefix/bin/pnpm"
NPM
  chmod +x "$RUNNER_TOOL_CACHE/node/bin/node" "$RUNNER_TOOL_CACHE/node/bin/npm"
  export PATH="$RUNNER_TOOL_CACHE/node/bin:$PATH"
}
! command -v node
! command -v npm
! command -v corepack
__crabbox_setup_pnpm 11.25.0
[ "$(pnpm --version)" = 11.25.0 ]
`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = []string{"PATH=" + bin, "RUNNER_TOOL_CACHE=" + filepath.Join(root, "tools"), "RUNNER_TEMP=" + filepath.Join(root, "tmp"), "NPM_ARGS=" + filepath.Join(root, "npm-args")}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pnpm bootstrap: %v\n%s", err, output)
	}
	args, err := os.ReadFile(filepath.Join(root, "npm-args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--global\n", "--ignore-scripts\n", "--no-audit\n", "--no-fund\n", "--registry=https://registry.npmjs.org\n", "pnpm@11.25.0\n"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("npm args missing %q: %s", want, args)
		}
	}
}

func TestLocalActionsSetupNodePreservesManagedPnpm(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	pnpmBin := filepath.Join(root, "tools", "pnpm", "bin")
	for _, dir := range []string{bin, pnpmBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	commands := map[string]string{
		"node":  "#!/bin/sh\nprintf '22.0.0\\n'\n",
		"uname": "#!/bin/sh\nprintf 'x86_64\\n'\n",
		"xz":    "#!/bin/sh\nexit 0\n",
		"curl": `#!/bin/sh
out=
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then shift; out="$1"; else url="$1"; fi
  shift
done
case "$url" in
  */index.tab) printf 'version\nv24.0.0\n' ;;
  */SHASUMS256.txt) printf '%s  node-v24.0.0-linux-x64.tar.xz\n' "$TEST_DIGEST" >"$out" ;;
  *) printf archive >"$out" ;;
esac
`,
		"sha256sum": "#!/bin/sh\nprintf '%s  %s\\n' \"$TEST_DIGEST\" \"$1\"\n",
		"tar": `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = -C ]; then shift; dir="$1"; fi
  shift
done
dir="$dir/node-v24.0.0-linux-x64/bin"
mkdir -p "$dir"
printf '#!/bin/sh\nprintf "24.0.0\\n"\n' >"$dir/node"
printf '#!/bin/sh\nprintf "wrong-corepack-pnpm\\n"\n' >"$dir/pnpm"
printf '#!/bin/sh\nexit 0\n' >"$dir/corepack"
chmod +x "$dir/node" "$dir/pnpm" "$dir/corepack"
`,
	}
	for name, script := range commands {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pnpmBin, "pnpm"), []byte("#!/bin/sh\nprintf '11.25.0\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "set -euo pipefail\n" + localActionsRuntimeShell() + `
__crabbox_setup_node 24
[ "$(node --version)" = 24.0.0 ]
[ "$(pnpm --version)" = 11.25.0 ]
`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+pnpmBin+":"+bin+":"+os.Getenv("PATH"), "GITHUB_WORKSPACE="+root,
		"RUNNER_TOOL_CACHE="+filepath.Join(root, "tools"), "RUNNER_TEMP="+filepath.Join(root, "tmp"), "TEST_DIGEST="+strings.Repeat("a", 64))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("setup-node displaced managed pnpm: %v\n%s", err, output)
	}
}

func TestRemoteEnsureLocalActionsRunEnvPersistsPnpmPath(t *testing.T) {
	for _, managedNode := range []bool{false, true} {
		name := "system node"
		if managedNode {
			name = "managed node"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			tools := filepath.Join(root, "tools")
			for _, tool := range []string{"node", "pnpm"} {
				if tool == "node" && !managedNode {
					continue
				}
				bin := filepath.Join(tools, tool, "bin")
				if err := os.MkdirAll(bin, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			envFile := filepath.Join(root, "run-env.sh")
			if err := os.WriteFile(envFile, []byte("export RUNNER_TOOL_CACHE="+shellQuote(tools)+"\nexport PATH=/usr/bin:/bin\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				cmd := exec.Command("bash", "-c", remoteEnsureLocalActionsRunEnv("cbx_123", envFile))
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("persist env: %v\n%s", err, output)
				}
			}
			content, err := os.ReadFile(envFile)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(content), "# CRABBOX_LOCAL_ACTIONS_PNPM_PATH") != 1 {
				t.Fatalf("pnpm path was not persisted exactly once: %s", content)
			}
			cmd := exec.Command("bash", "--noprofile", "--norc", "-c", ". "+shellQuote(envFile)+"; command -v pnpm")
			output, err := cmd.CombinedOutput()
			if err != nil || strings.TrimSpace(string(output)) != filepath.Join(tools, "pnpm", "bin", "pnpm") {
				t.Fatalf("fresh run pnpm=%q err=%v", output, err)
			}
		})
	}
}
