# Security Policy

## Purpose and trust model

Crabbox is a developer execution tool. It is designed for one trusted local OS
user and, when a coordinator is used, a cooperative team of trusted operators.
It is not designed to isolate mutually adversarial tenants, hostile users on a
shared host, or untrusted operators behind one coordinator.

Treat repository configuration as executable project automation, like a
Makefile, package script, or CI workflow. It can select providers and local
helper binaries, run commands, configure runtime arguments, mount host
resources, and forward explicitly selected environment values. Review an
unfamiliar repository and its Crabbox configuration before running it.

Crabbox-created containers, virtual machines, portals, and provider resources
are development execution environments, not a uniform security sandbox.
Isolation depends on the selected provider and its documented behavior. In
particular, local container socket pass-through gives the lease authority over
the host container engine.

The coordinator's authentication, ownership, and sharing controls remain
product boundaries. They prevent unauthorized access and accidental
cross-owner operations in the supported trusted-team model, but they are not a
hostile multi-tenant isolation claim.

See [Operational security](docs/security.md) for deployment guidance.

## Supported versions

Security fixes target the latest release and current `main`. Older releases do
not receive a guaranteed backport or long-term support window.

## In scope

Examples of reports that cross a supported Crabbox boundary:

- authentication bypass without a valid configured credential;
- escalation from a non-admin identity to documented admin capabilities;
- access to another owner's resources contrary to documented authorization;
- Crabbox silently sending a trusted credential to a destination selected by a
  different, lower-trust configuration source;
- generated credentials being exposed beyond the current OS user by Crabbox;
- destructive provider actions against resources Crabbox cannot strongly
  identify as its own;
- integrity failures in artifacts or images that Crabbox downloads and installs
  as part of a documented default workflow;
- command injection outside an explicitly configured command, helper, runtime,
  or project-automation surface.

## Hardening and expected behavior

The following are normally hardening, reliability, or documented behavior
rather than security vulnerabilities:

- exposure to another process running as the same trusted OS user;
- short-lived credentials in local browser history or local process metadata;
- repository-configured commands, helpers, runtime arguments, guest
  credentials, mounts, or container-engine access;
- an admin credential exercising admin capabilities or choosing attribution;
- trusted operators deliberately attacking each other;
- public network reachability protected by documented key or token
  authentication;
- missing orphan cleanup, reconciliation, or cost-control coverage;
- crashes or denial of service caused by a trusted operator or trusted project
  configuration;
- defense-in-depth and supply-chain improvements without a demonstrated bypass
  of a supported boundary.

Hardening is welcome when it preserves documented workflows. Prefer warnings,
source-bound credentials, explicit operator consent, and opt-in stricter modes
over silently removing useful capabilities. Changes that require a compatibility
break need an explicit product decision and migration path.

## Out of scope

- hostile repository configuration or project scripts;
- another process or user with access to the same OS account;
- mutually adversarial users sharing one coordinator, shared token, pond, or
  local host;
- compromise of a configured provider, helper binary, container engine, image,
  or external command;
- secrets passed to commands or machines through an explicit allowlist or
  project configuration;
- arbitrary commands executed on a leased machine;
- leaked credentials outside Crabbox's control.

## Reporting

Use [GitHub private vulnerability reporting](https://github.com/openclaw/crabbox/security/advisories/new)
for a suspected vulnerability. Use a normal GitHub issue for non-sensitive
hardening, reliability, documentation, or expected-behavior discussions.

Do not publish live credentials, private infrastructure details, or an
exploit-ready report containing sensitive deployment information. Reports
should identify the supported boundary crossed, required attacker access,
affected code path, and a reproduction where practical. CVSS scoring is useful
only after the report is accepted as a vulnerability.
