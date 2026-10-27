# Windows managed-host proof snapshot

These are the Python drivers used for the 2026-10-27 ARM64 proof. They are
an inspectable proof snapshot, not a production host installer. They exercise
the shipped CLI plus the real Node/PostgreSQL coordinator, outbound relay,
host agent, pinned MXC executor, and nested SSH endpoint.

Use a **designated Windows proof target** with an active desktop session,
working key-only SSH/SCP, and the pinned MXC 0.8.0 wxc-exec.exe plus its
plm.exe companion. The Windows account must be able to create a task-local
directory and an Interactive scheduled task. Neither SSH network logon nor an
S4U task supplies MXC activation. The proof does not change system SSH,
firewall, execution policy, or installed applications. It creates disposable
MXC borrower accounts and deliberately terminates its own launcher/agent.

## Inputs and preparation

On the macOS/Linux controller, provide these environment variables:

| Variable | Value |
| --- | --- |
| CRABBOX_PROOF_REPO | Absolute path to the frozen Crabbox checkout |
| CRABBOX_PROOF_EVIDENCE | New private directory outside the checkout |
| CRABBOX_PROOF_SSH_CONFIG | Private OpenSSH config with a pinned known-host entry and identity |
| CRABBOX_PROOF_SSH_HOST | Target alias from that config |
| CRABBOX_PROOF_MXC | Local pinned ARM64 executor; plm.exe is alongside it |

Requirements: Python 3, Docker, Go from go.mod, Node from .node-version,
and coordinator dependencies. Use an SSH user profile path without spaces for
this snapshot's scheduled-task launcher. Remote loopback port 48763 must be
unused. The coordinator is published only on a dynamically allocated local
loopback port and reached by the Windows agent through a loopback SSH tunnel.
Synthetic tokens are generated into private files; never publish the evidence
directory wholesale.

From the frozen repository:

~~~sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
GOOS=windows GOARCH=arm64 go build -trimpath -o bin/crabbox-host-windows.exe ./cmd/crabbox
npm ci --prefix worker
npm run build:node --prefix worker
python3 docs/diagnostics/managed-host-windows-harness/driver.py setup
python3 docs/diagnostics/managed-host-windows-harness/qualify.py
python3 docs/diagnostics/managed-host-windows-harness/pool.py
python3 docs/diagnostics/managed-host-windows-harness/cleanup.py
~~~

Setup creates a task-specific Node/PostgreSQL network, guest directory,
Interactive scheduled task, and enrollment. It clones an anonymous public Git
fixture and adds hello.txt for sync proof. Qualification runs allowed execution,
foreign relay denial, wrong endpoint key denial with an absent effect file,
a retained non-PTY shell, a detached writing process, exact MXC launcher
termination, revocation, fresh-owner key rotation, and crash/restart recovery.
The pool phase checks two portable-pool generations and unattended expiry.
Cleanup refuses to discard uncertain custody; it requires an available host
and no retained runtime before deleting only its resources.

If a phase fails, inspect the private receipts, use the ordinary crabbox stop
path against **this isolated coordinator**, and restart its Interactive agent
when recovering crash custody. Do not erase unknown MXC accounts or delete
state to make cleanup appear successful. Run cleanup only after settlement.
Partial checks are recorded; a failed phase does not prove later checks.

## Native regressions

Cross-compile test binaries, transfer them to the designated Windows target,
then run these filters there (PowerShell arguments containing dots are quoted):

~~~powershell
$env:PSExecutionPolicyPreference = 'Bypass' # current process and test children only
& .\sshhost.test.exe '-test.run=TestMXCEndpointCommandThroughCMD' '-test.v'
& .\cli.test.exe '-test.run=TestWindowsNativeStdin(Runtime|Cancellation|SynchronousPipe)$|TestWindowsSSHStdinReaderBehavior$' '-test.timeout=3m' '-test.v'
~~~

Build them with GOOS=windows GOARCH=arm64 go test -c -o OUTPUT
./internal/providers/sshhost and the same command for ./internal/cli.
The synchronous-input test checks exact bytes and a trailing unread byte;
existing async tests cover pending reads, partial reads, EOF, cancellation,
short input, and destination errors. The launch test goes through the real
cmd.exe /c wrapper, including spaces, metacharacters, Unicode quotes, and exit
status. Both new regressions were also run with only their production fixes
reverted and failed.

Publish only reviewed/sanitized result JSON, build digests, and necessary
terminal excerpts. Keep target addresses, SSH config, tokens, private keys,
guest profile names, and raw agent state private.
