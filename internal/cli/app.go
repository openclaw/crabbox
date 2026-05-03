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
			next := append([]string{}, args[1:]...)
			next = append(next, "--help")
			return a.Run(ctx, next)
		}
		a.printHelp()
		return nil
	case "-v", "--version", "version":
		fmt.Fprintln(a.Stdout, version)
		return nil
	case "doctor":
		return a.doctor(ctx, args[1:])
	case "login":
		return a.login(ctx, args[1:])
	case "logout":
		return a.logout(ctx, args[1:])
	case "whoami":
		return a.whoami(ctx, args[1:])
	case "admin":
		return a.admin(ctx, args[1:])
	case "history":
		return a.history(ctx, args[1:])
	case "logs":
		return a.logs(ctx, args[1:])
	case "events":
		return a.events(ctx, args[1:])
	case "attach":
		return a.attach(ctx, args[1:])
	case "results":
		return a.results(ctx, args[1:])
	case "cache":
		return a.cache(ctx, args[1:])
	case "config":
		return a.config(ctx, args[1:])
	case "init":
		return a.initProject(ctx, args[1:])
	case "image":
		return a.image(ctx, args[1:])
	case "pool":
		return a.pool(ctx, args[1:])
	case "machine":
		return a.machine(ctx, args[1:])
	case "list":
		return a.list(ctx, args[1:])
	case "usage":
		return a.usage(ctx, args[1:])
	case "actions":
		return a.actions(ctx, args[1:])
	case "cleanup":
		return a.cleanup(ctx, args[1:])
	case "warmup":
		return a.warmup(ctx, args[1:])
	case "run":
		return a.runCommand(ctx, args[1:])
	case "sync-plan":
		return a.syncPlan(ctx, args[1:])
	case "status":
		return a.status(ctx, args[1:])
	case "ssh":
		return a.ssh(ctx, args[1:])
	case "vnc":
		return a.vnc(ctx, args[1:])
	case "inspect":
		return a.inspect(ctx, args[1:])
	case "stop", "release":
		return a.stop(ctx, args[1:])
	default:
		return exit(2, "unknown command %q", args[0])
	}
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
  crabbox warmup --desktop --browser
      Lease a UI-capable box with a browser.

Commands:
  init        Onboard the current repo for Crabbox
  login       Open GitHub login, store broker credentials, verify access
  logout      Remove the stored broker token
  whoami      Show broker identity
  doctor      Check local and broker/provider readiness
  warmup      Lease a box and wait until it is ready
  run         Sync the repo, run a remote command, stream output
  sync-plan   Show local sync manifest size hotspots
  history     List recorded remote runs
  logs        Print recorded run logs
  events      Print recorded run events
  attach      Follow recorded events for an active run
  results     Show recorded test result summaries
  cache       Inspect, purge, or warm remote caches
  status      Show lease state; add --wait to block until ready
  list        List Crabbox machines
  image       Create or promote brokered AWS runner images
  usage       Show cost and usage estimates by user, org, or fleet
  admin       Lease admin controls for trusted operators
  actions     Register GitHub Actions runners or dispatch workflows
  ssh         Print the SSH command for a lease
  vnc         Print or open VNC connection details for a desktop lease
  inspect     Print lease/provider details; add --json for scripts
  stop        Release a lease or delete a direct-provider machine
  cleanup     Sweep expired direct-provider machines
  config      Show or update user config

Common Flows:
  crabbox run --class beast -- pnpm check
  crabbox warmup
  crabbox status --id blue-lobster --wait
  crabbox run --id blue-lobster --shell 'pnpm install --frozen-lockfile && pnpm test'
  crabbox ssh --id blue-lobster
  crabbox vnc --id blue-lobster --open
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
  crabbox login [--url <url>] [--provider aws|hetzner] [--no-browser]
  crabbox login --url <url> --token-stdin [--provider aws|hetzner]
  crabbox config path
  crabbox config show [--json]
  crabbox config set-broker --url <url> --token-stdin [--provider aws|hetzner]

Environment:
  CRABBOX_COORDINATOR          Broker URL
  CRABBOX_COORDINATOR_TOKEN    Broker bearer token
  CRABBOX_COORDINATOR_ADMIN_TOKEN
                               Broker admin bearer token
  CRABBOX_ACCESS_CLIENT_ID     Cloudflare Access service token client ID
  CRABBOX_ACCESS_CLIENT_SECRET Cloudflare Access service token client secret
  CRABBOX_ACCESS_TOKEN         Cloudflare Access JWT for protected routes
  CRABBOX_PROVIDER             hetzner, aws, ssh, or blacksmith-testbox
  CRABBOX_TARGET               linux, macos, or windows
  CRABBOX_WINDOWS_MODE         normal or wsl2
  CRABBOX_DESKTOP              Provision or require desktop/VNC capability
  CRABBOX_BROWSER              Provision or require browser capability
  CRABBOX_STATIC_HOST          Static SSH host for provider=ssh
  CRABBOX_STATIC_MANAGED_LOGIN Enable static managed VNC login where supported
  CRABBOX_STATIC_MANAGED_USER  Static managed VNC login user
  CRABBOX_OWNER                Usage owner override
  CRABBOX_ORG                  Usage org override
  CRABBOX_CONFIG               Optional config path
  CRABBOX_IDLE_TIMEOUT         Default idle expiry, e.g. 30m
  CRABBOX_TTL                  Maximum lease lifetime, e.g. 90m
  CRABBOX_AWS_REGION           Default eu-west-1
  CRABBOX_AWS_SSH_CIDRS        Comma-separated AWS SSH source CIDRs
  CRABBOX_SSH_FALLBACK_PORTS   Comma-separated SSH fallback ports, or none
  CRABBOX_CAPACITY_MARKET      spot or on-demand
  CRABBOX_CAPACITY_REGIONS     Comma-separated AWS Spot placement candidates
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
