package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (a App) initProject(_ context.Context, args []string) error {
	fs := newFlagSet("init", a.Stderr)
	force := fs.Bool("force", false, "overwrite generated files")
	detect := fs.Bool("detect", false, "detect repo test commands and write a jobs.detected entry")
	workflow := fs.String("workflow", ".github/workflows/crabbox.yml", "workflow path")
	skill := fs.String("skill", ".agents/skills/crabbox/SKILL.md", "agent skill path")
	config := fs.String("config", ".crabbox.yaml", "repo config path")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	detected := initProjectDetection{}
	if *detect {
		detected = detectInitProject(repo.Root)
	}
	files := map[string]string{
		filepath.Join(repo.Root, *config):   projectConfigTemplate(repo.Name, detected),
		filepath.Join(repo.Root, *workflow): workflowTemplate(),
		filepath.Join(repo.Root, *skill):    skillTemplate(detected),
	}
	for path, content := range files {
		if err := writeInitFile(path, content, *force); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "wrote %s\n", path)
	}
	if *detect {
		if len(detected.Commands) == 0 {
			fmt.Fprintln(a.Stdout, "detected no runnable project commands; edit .crabbox.yaml jobs manually")
		} else {
			fmt.Fprintf(a.Stdout, "detected job: crabbox job run detected\n")
		}
	}
	return nil
}

func writeInitFile(path, content string, force bool) error {
	if _, err := os.Stat(path); err == nil && !force {
		return exit(2, "%s already exists; use --force to overwrite", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return exit(2, "create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return exit(2, "write %s: %v", path, err)
	}
	return nil
}

type initProjectDetection struct {
	Commands       []string
	PreflightTools []string
	SyncExcludes   []string
	EnvAllow       []string
}

func projectConfigTemplate(repoName string, detected initProjectDetection) string {
	syncExcludes := appendUniqueStrings([]string{
		".cache",
		".turbo",
		"dist",
		"node_modules",
	}, detected.SyncExcludes...)
	envAllow := appendUniqueStrings([]string{"CI", "NODE_OPTIONS"}, detected.EnvAllow...)

	var b strings.Builder
	fmt.Fprintf(&b, `profile: %s-check
class: beast
capacity:
  market: spot
  strategy: most-available
  fallback: on-demand-after-120s
actions:
  workflow: .github/workflows/crabbox.yml
  job: hydrate
  runnerLabels:
    - crabbox
  runnerVersion: latest
  ephemeral: true
sync:
  delete: true
  checksum: false
  gitSeed: true
  fingerprint: true
  timeout: 15m
  warnFiles: 50000
  warnBytes: 5368709120
  failFiles: 150000
  failBytes: 21474836480
  exclude:
`, repoName)
	writeYAMLList(&b, syncExcludes, 4)
	if len(detected.PreflightTools) > 0 {
		b.WriteString("run:\n")
		b.WriteString("  preflightTools:\n")
		writeYAMLList(&b, detected.PreflightTools, 4)
	}
	b.WriteString(`env:
  allow:
`)
	writeYAMLList(&b, envAllow, 4)
	b.WriteString(`ssh:
  user: crabbox
  port: "2222"
  # Ordered fallback ports tried after ssh.port; use [] to disable fallback.
  fallbackPorts:
    - "22"
cache:
  # Optional provider-backed cache volumes. Keep paths outside the synced source tree.
  volumes: []
`)
	if len(detected.Commands) > 0 {
		b.WriteString("jobs:\n")
		b.WriteString("  detected:\n")
		b.WriteString("    shell: true\n")
		b.WriteString("    command: >\n")
		for i, command := range detected.Commands {
			line := command
			if i < len(detected.Commands)-1 {
				line += " &&"
			}
			fmt.Fprintf(&b, "      %s\n", line)
		}
		b.WriteString("    stop: auto\n")
	}
	return b.String()
}

func writeYAMLList(b *strings.Builder, values []string, indent int) {
	prefix := strings.Repeat(" ", indent)
	for _, value := range values {
		fmt.Fprintf(b, "%s- %s\n", prefix, yamlScalar(value))
	}
}

func yamlScalar(value string) string {
	if strings.TrimSpace(value) == "" {
		return `""`
	}
	if strings.ContainsAny(value, ":#[]{}&,*!?|>'\"%@\t`") {
		return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	}
	return value
}

func workflowTemplate() string {
	return `name: crabbox

on:
  workflow_dispatch:
    inputs:
      ref:
        description: "Git ref to hydrate"
        required: false
        type: string
      crabbox_id:
        description: "Crabbox lease ID"
        required: true
        type: string
      crabbox_runner_label:
        description: "Dynamic Crabbox runner label"
        required: true
        type: string
      crabbox_job:
        description: "Hydration job identifier expected by Crabbox"
        required: false
        default: "hydrate"
        type: string
      crabbox_keep_alive_minutes:
        description: "Minutes to keep the hydrated job alive"
        required: false
        default: "90"
        type: string

permissions:
  contents: read

jobs:
  hydrate:
    runs-on: [self-hosted, "${{ inputs.crabbox_runner_label }}"]
    timeout-minutes: 120
    steps:
      - uses: actions/checkout@v6
        with:
          ref: ${{ inputs.ref || github.ref }}
      - name: Hydrate
        run: |
          if [ -f package-lock.json ]; then npm ci; fi
          if [ -f pnpm-lock.yaml ]; then corepack enable && pnpm install --frozen-lockfile; fi
          if [ -f go.mod ]; then go mod download; fi
      - name: Mark Crabbox ready
        shell: bash
        env:
          CRABBOX_ID: ${{ inputs.crabbox_id }}
          CRABBOX_JOB: ${{ inputs.crabbox_job }}
        run: |
          case "$CRABBOX_ID" in
            ""|*[!A-Za-z0-9_-]*)
              echo "::error::crabbox_id must match [A-Za-z0-9_-]+"
              exit 2
              ;;
          esac
          case "$CRABBOX_JOB" in
            *$'\n'*|*$'\r'*)
              echo "::error::crabbox_job must not contain line breaks"
              exit 2
              ;;
          esac
          job="$CRABBOX_JOB"
          if [ -z "$job" ]; then job=hydrate; fi
          mkdir -p "$HOME/.crabbox/actions"
          state="$HOME/.crabbox/actions/${CRABBOX_ID}.env"
          env_file="$HOME/.crabbox/actions/${CRABBOX_ID}.env.sh"
          services_file="$HOME/.crabbox/actions/${CRABBOX_ID}.services"
          write_export() {
            key="$1"
            value="${!key-}"
            if [ -n "$value" ]; then
              printf 'export %s=%q\n' "$key" "$value"
            fi
          }
          {
            for key in CI GITHUB_ACTIONS GITHUB_WORKSPACE GITHUB_REPOSITORY GITHUB_RUN_ID GITHUB_RUN_NUMBER GITHUB_RUN_ATTEMPT GITHUB_REF GITHUB_REF_NAME GITHUB_SHA GITHUB_EVENT_NAME GITHUB_ACTOR GITHUB_JOB RUNNER_OS RUNNER_ARCH RUNNER_TEMP RUNNER_TOOL_CACHE; do
              write_export "$key"
            done
          } > "${env_file}.tmp"
          mv "${env_file}.tmp" "$env_file"
          {
            echo "# Docker containers visible from the hydrated runner"
            docker ps --format '{{.Names}}\t{{.Image}}\t{{.Ports}}' 2>/dev/null || true
          } > "${services_file}.tmp"
          mv "${services_file}.tmp" "$services_file"
          tmp="${state}.tmp"
          commit="$(git -C "$GITHUB_WORKSPACE" rev-parse HEAD)"
          {
            echo "WORKSPACE=${GITHUB_WORKSPACE}"
            echo "COMMIT=${commit}"
            echo "RUN_ID=${GITHUB_RUN_ID}"
            echo "JOB=${job}"
            echo "ENV_FILE=${env_file}"
            echo "SERVICES_FILE=${services_file}"
            echo "READY_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
          } > "$tmp"
          mv "$tmp" "$state"
      - name: Keep Crabbox job alive
        shell: bash
        env:
          CRABBOX_ID: ${{ inputs.crabbox_id }}
          CRABBOX_KEEP_ALIVE_MINUTES: ${{ inputs.crabbox_keep_alive_minutes }}
        run: |
          case "$CRABBOX_ID" in
            ""|*[!A-Za-z0-9_-]*)
              echo "::error::crabbox_id must match [A-Za-z0-9_-]+"
              exit 2
              ;;
          esac
          minutes="$CRABBOX_KEEP_ALIVE_MINUTES"
          case "$minutes" in
            ''|*[!0-9]*) minutes=90 ;;
          esac
          stop="$HOME/.crabbox/actions/${CRABBOX_ID}.stop"
          deadline=$(( $(date +%s) + minutes * 60 ))
          while [ "$(date +%s)" -lt "$deadline" ]; do
            if [ -f "$stop" ]; then
              exit 0
            fi
            sleep 15
          done
`
}

func skillTemplate(detected initProjectDetection) string {
	var b strings.Builder
	b.WriteString(`# Crabbox

Use Crabbox for remote Linux verification.

Workflow:
- Warm early: crabbox warmup
- Reuse the returned slug for interactive checks and keep the cbx_ id in scripts/logs.
- Run checks with crabbox run --id <slug> -- <command>.
- Use --cache-volume [name=]key:path only when the selected provider supports provider-backed cache volumes.
- Use crabbox status --id <slug> --wait before broad gates if needed.
- Use crabbox ssh --id <slug> to inspect the runner when a failure needs live context.
- Stop with crabbox stop <slug> when finished.

Do not debug product failures on a reused box that fails sync sanity. Stop it, warm a fresh box, and rerun.
`)
	if len(detected.Commands) > 0 {
		b.WriteString("\nDetected workflow:\n- Prefer crabbox job run detected for the broad remote check.\n\n```sh\ncrabbox job run detected\n```\n")
	}
	return b.String()
}
