package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

type App struct {
	Stdout io.Writer
	Stderr io.Writer
}

func Run(ctx context.Context, args []string) error {
	app := App{Stdout: os.Stdout, Stderr: os.Stderr}
	return app.Run(ctx, args)
}

func (a App) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		a.printHelp()
		return exit(2, "missing command")
	}

	switch args[0] {
	case "-h", "--help":
		a.printHelp()
		return nil
	case "help":
		if len(args) > 1 {
			return a.runKong(ctx, args)
		}
		a.printHelp()
		return nil
	}
	if help, ok := a.directCommandHelp(ctx, args); ok {
		return help
	}

	return a.runKong(ctx, args)
}

func (a App) directCommandHelp(ctx context.Context, args []string) (error, bool) {
	if len(args) < 2 || !isHelpArg(args[1]) || isKongCommandGroup(args[0]) {
		return nil, false
	}
	helpArgs := []string{"--help"}
	switch args[0] {
	case "init":
		return a.initProject(ctx, helpArgs), true
	case "login":
		return a.login(ctx, helpArgs), true
	case "logout":
		return a.logout(ctx, helpArgs), true
	case "whoami":
		return a.whoami(ctx, helpArgs), true
	case "doctor":
		return a.doctor(ctx, helpArgs), true
	case "warmup":
		return a.warmup(ctx, helpArgs), true
	case "run":
		return a.runCommand(ctx, helpArgs), true
	case "job":
		return nil, false
	case "sync-plan":
		return a.syncPlan(ctx, helpArgs), true
	case "history":
		return a.history(ctx, helpArgs), true
	case "logs":
		return a.logs(ctx, helpArgs), true
	case "events":
		return a.events(ctx, helpArgs), true
	case "attach":
		return a.attach(ctx, helpArgs), true
	case "results":
		return a.results(ctx, helpArgs), true
	case "status":
		return a.status(ctx, helpArgs), true
	case "list":
		return a.list(ctx, helpArgs), true
	case "usage":
		return a.usage(ctx, helpArgs), true
	case "ssh":
		return a.ssh(ctx, helpArgs), true
	case "vnc":
		return a.vnc(ctx, helpArgs), true
	case "webvnc":
		return a.webvnc(ctx, helpArgs), true
	case "code":
		return a.webCode(ctx, helpArgs), true
	case "egress":
		return a.egress(ctx, helpArgs), true
	case "screenshot":
		return a.screenshot(ctx, helpArgs), true
	case "artifacts":
		return nil, false
	case "inspect":
		return a.inspect(ctx, helpArgs), true
	case "stop", "release":
		return a.stop(ctx, helpArgs), true
	case "cleanup":
		return a.cleanup(ctx, helpArgs), true
	default:
		return nil, false
	}
}

func isHelpArg(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "help"
}

func (a App) printHelp() {
	fmt.Fprintln(a.Stdout, `Crabbox leases remote test boxes, syncs your dirty checkout, runs commands, and cleans up.

Usage:
  crabbox <command> [flags]
  crabbox run [flags] -- <command...>

Start Here:
  crabbox login
      Open GitHub login and store broker credentials.
  crabbox doctor
      Check local tools, config, broker, and provider access.
  crabbox init
      Add repo-local Crabbox config, GitHub workflow, and agent skill.
  crabbox warmup --class beast
      Lease a reusable box and print a cbx_... id plus friendly slug.
  crabbox run --id blue-lobster -- pnpm test:changed
      Sync this checkout to the box and run a command.
  crabbox warmup --desktop --browser --code
      Lease a UI-capable box with a browser and web code editor.

Commands:
  init        Onboard the current repo for Crabbox
  login       Open GitHub login, store broker credentials, verify access
  logout      Remove the stored broker token
  whoami      Show broker identity
  doctor      Check local and broker/provider readiness
  warmup      Lease a box and wait until it is ready
  run         Sync the repo, run a remote command, stream output
  job         Run named repo-local Crabbox jobs
  desktop     Launch apps into a visible desktop session
  media       Create preview artifacts from recorded desktop videos
  artifacts   Collect, transform, and publish QA artifacts
  sync-plan   Show local sync manifest size hotspots
  history     List recorded remote runs
  logs        Print recorded run logs
  events      Print recorded run events
  attach      Follow recorded events for an active run
  results     Show recorded test result summaries
  cache       Inspect, purge, or warm remote caches
  status      Show lease state; add --wait to block until ready
  list        List Crabbox machines
  share       Share a lease with users or the owning org
  unshare     Remove lease sharing
  image       Create or promote brokered AWS runner images
  usage       Show cost and usage estimates by user, org, or fleet
  admin       Lease admin controls for trusted operators
  actions     Register GitHub Actions runners or dispatch workflows
  ssh         Print the SSH command for a lease
  vnc         Print or open VNC connection details for a desktop lease
  webvnc      Bridge a desktop lease into the authenticated web portal
  code        Bridge a code lease into the authenticated web portal
  egress      Bridge lease browser/app traffic through this machine
  screenshot  Capture a PNG from a desktop lease
  inspect     Print lease/provider details; add --json for scripts
  stop        Release a lease or delete a direct-provider machine
  cleanup     Sweep expired direct-provider machines or local provider state
  azure       Azure provider setup and login
  config      Show or update user config

Common Flows:
  crabbox run --class beast -- pnpm check
  crabbox job run openclaw-wsl2
  crabbox warmup
  crabbox status --id blue-lobster --wait
  crabbox run --id blue-lobster --shell 'pnpm install --frozen-lockfile && pnpm test'
  crabbox ssh --id blue-lobster
  crabbox vnc --id blue-lobster --open
  crabbox desktop launch --id blue-lobster --browser --url https://example.com --webvnc --open
  crabbox desktop proof --id blue-lobster --output artifacts/blue-lobster-proof -- ./scripts/visual-smoke.sh
  crabbox media preview --input desktop.mp4 --output desktop-preview.gif --trimmed-video-output desktop-change.mp4
  crabbox artifacts collect --id blue-lobster --all --output artifacts/blue-lobster
  crabbox artifacts publish --pr 123 --dir artifacts/blue-lobster --storage s3 --bucket qa-artifacts
  crabbox webvnc --id blue-lobster --open
  crabbox code --id blue-lobster --open
  crabbox egress start --id blue-lobster --profile discord --daemon
  crabbox share --id blue-lobster --user friend@example.com
  crabbox share --id blue-lobster --org
  crabbox screenshot --id blue-lobster --output desktop.png
  crabbox inspect --id blue-lobster --json
  crabbox history --lease cbx_abcdef123456
  crabbox logs run_123
  crabbox events run_123
  crabbox attach run_123
  crabbox results run_123
  crabbox cache stats --id blue-lobster
  crabbox usage --scope org
  crabbox admin leases --state active
  crabbox warmup --actions-runner
  crabbox actions hydrate --id blue-lobster
  crabbox actions dispatch -f testbox_id=cbx_abcdef123456
  crabbox run --provider ssh --target macos --static-host mac.local -- echo ok
  crabbox run --provider ssh --target windows --windows-mode normal --static-host win.local -- pwsh -NoProfile -Command '$PSVersionTable'
  crabbox stop blue-lobster

Global:
  -h, --help     Show help
  --version      Print version

Config:
  crabbox login [--url <url>] [--provider aws|azure|hetzner] [--no-browser]
  crabbox login --url <url> --token-stdin [--provider aws|azure|hetzner]
  crabbox azure login [--subscription <id>] [--location <loc>] [--json]
  crabbox config path
  crabbox config show [--json]
  crabbox config set-broker --url <url> --token-stdin [--provider aws|azure|hetzner]

Environment:
  CRABBOX_COORDINATOR          Broker URL
  CRABBOX_COORDINATOR_TOKEN    Broker bearer token
  CRABBOX_COORDINATOR_ADMIN_TOKEN
                               Broker admin bearer token
  CRABBOX_ACCESS_CLIENT_ID     Cloudflare Access service token client ID
  CRABBOX_ACCESS_CLIENT_SECRET Cloudflare Access service token client secret
  CRABBOX_ACCESS_TOKEN         Cloudflare Access JWT for protected routes
  CRABBOX_PROVIDER             hetzner, aws, azure, ssh, blacksmith-testbox, namespace-devbox, semaphore, daytona, islo, e2b, or sprites
  CRABBOX_TARGET               linux, macos, or windows
  CRABBOX_WINDOWS_MODE         normal or wsl2
  CRABBOX_DESKTOP              Provision or require desktop/VNC capability
  CRABBOX_BROWSER              Provision or require browser capability
  CRABBOX_CODE                 Provision or require web code capability
  CRABBOX_STATIC_HOST          Static SSH host for provider=ssh
  CRABBOX_OWNER                Usage owner override
  CRABBOX_ORG                  Usage org override
  CRABBOX_CONFIG               Optional config path
  CRABBOX_IDLE_TIMEOUT         Default idle expiry, e.g. 30m
  CRABBOX_TTL                  Maximum lease lifetime, e.g. 90m
  CRABBOX_AWS_REGION           Default eu-west-1
  CRABBOX_AWS_SSH_CIDRS        Comma-separated AWS SSH source CIDRs
  CRABBOX_SSH_FALLBACK_PORTS   Comma-separated SSH fallback ports, or none
  CRABBOX_CAPACITY_MARKET      spot or on-demand
  CRABBOX_CAPACITY_REGIONS     Comma-separated AWS region fallback candidates
  HCLOUD_TOKEN/HETZNER_TOKEN   Direct Hetzner mode

Aliases:
  crabbox release <id-or-slug> Alias for stop
  crabbox pool list            Alias for list
  crabbox machine cleanup      Alias for cleanup

Docs:
  docs/commands/README.md`)
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitError{Code: 0}
		}
		return exit(2, "%v", err)
	}
	return nil
}
