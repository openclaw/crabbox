# Provider Live Smoke

Read when:

- adding or reviewing a provider that needs external credentials, quota, local
  hypervisor access, or a self-hosted control plane;
- deciding whether offline tests are enough for a provider PR;
- writing an opt-in live validation command for a provider doc.

Most provider work must be provable without live credentials. Unit tests should
cover config, command construction, JSON parsing, lifecycle decisions, and error
paths. A live smoke is the extra opt-in proof that the documented real substrate
still matches the offline contract.

## Pick Candidates

Start with the checked-in capability matrix:

```sh
crabbox providers recommend live-smoke
crabbox providers recommend offline-validation
crabbox providers recommend cost-control
crabbox providers --json
```

`providers recommend live-smoke` ranks providers that expose enough sync,
cleanup, lifecycle, or evidence metadata to be worth spending real capacity.
It does not prove credentials, quota, regions, templates, Kubernetes contexts, or
provider-side availability. Run `doctor` before creating resources:

```sh
crabbox doctor --provider <name>
```

## Smoke Contract

Every live smoke should prove the narrowest real behavior that offline tests
cannot:

- **SSH lease providers**: acquire or resolve one lease, wait for SSH, sync a
  tiny checkout, run `true` or a small repository command, then release or
  cleanup the lease.
- **Delegated run providers**: create or reuse one provider-owned runtime, send a
  tiny command, stream or collect the result, record any session/proof/output
  metadata the provider advertises, then stop or cleanup when the lifecycle
  claims cleanup.
- **Service-control providers**: inspect, start, stop, or redeploy the named
  service without claiming arbitrary command execution.
- **Local runtimes**: prove host prerequisite detection, create one disposable
  runtime, run a tiny command, and delete it. Local smokes are still opt-in when
  they mutate local VM, container, hypervisor, or sandbox state.
- **BYO or external providers**: prove the documented handoff contract only:
  stable host ID, SSH target or external lease metadata, command execution, and
  cleanup semantics if Crabbox owns cleanup.

Do not turn a live smoke into an integration suite. The goal is to prove the
provider boundary, not the provider's whole product.

## Evidence To Keep

A useful smoke leaves enough output to debug a failed adapter without leaking
secrets:

- provider, target, region or local runtime when relevant;
- lease ID, slug, session ID, or service ID when one exists;
- command exit code and timing summary;
- proof, artifact, download, preview URL, or cleanup command when the provider
  advertises that capability;
- exact cleanup outcome.

Scrub tokens, personal paths, private hostnames, and private IP addresses before
copying smoke output into an issue, PR, or fixture.

## Provider Docs

Each provider that needs real credentials should document an opt-in smoke with:

- required CLI or SDK authentication;
- required env/config variables;
- quota, cost, local mutation, or cleanup risk;
- the smallest command that proves the provider contract;
- the cleanup command to run if the smoke is interrupted.

When live credentials are unavailable, land the offline tests and docs first.
Mark the live smoke as opt-in instead of weakening the provider contract or
pretending an untested live path is proven.
