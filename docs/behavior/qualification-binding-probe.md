# Stateless qualification service-binding probe

This optional diagnostic in `scripts/image-qualification-control.mjs` characterizes
Cloudflare service-binding props. It does not qualify privileged bindings, change
normal bootstrap, or measure ownership performance. Existing qualification
controller/candidate/authority ownership checks remain strict and unchanged.
No existing scheduled workflow invokes this mode.

The fixture has two new, exact-named Workers: a receiver exporting `BindingProbe`
with no resource bindings, and a caller bound only to that receiver and an
ephemeral authentication secret. The receiver echoes synthetic `ctx.props`; its
HTTP handler returns 403. The caller accepts only authenticated `POST /observe`,
forwards no request input, and returns the receiver's reply. Neither Worker has
AWS credentials, state, namespaces, authority access, arbitrary URL forwarding,
or production/campaign data. Public names, modules and sentinel props are not
secrets. The invocation key is random, held in process memory and never journaled.

## Admission and source preparation

The commands below describe a future operator-admitted cycle, not permission to
execute one. Adopt reviewed tooling through the existing protected qualification
source process first. Existing credential custody and current item access
instructions must explicitly admit this diagnostic. A local source pin is an
identity check, not a replacement for protected-source admission or authorization.
The command requires a clean Git checkout at that exact pin. No new credential
owner, coordinator or automated cleanup service is introduced.

Prepare a public plan without loading the API token:

```sh
export QUALIFICATION_BINDING_PROBE_SOURCE_SHA=<reviewed-adopted-commit>
export CLOUDFLARE_ACCOUNT_ID=<admitted-account-id>
node scripts/image-qualification-control.mjs binding-probe-plan \
  /private/evidence/plan.json <expected-token-id> <12-hex-random-nonce> <UTC-start-time>
```

Freeze the resulting plan, module hashes, exact names, custody, invocation exposure,
resource bound and fallback operator before admitting execution. Use a private
operator-owned journal directory, outside the checkout, on the same durable host
available to recovery. Review collision and complete inventory results before any
retry; the run command is deliberately single-use. Current credential instructions
must be read before any credential use. Supply only the existing Cloudflare API
token/account environment through the supported secret injector, never argv or a
plaintext cache. Account-token verification must match the frozen token ID,
active state and expiry beyond the cycle plus recovery reserve.

```sh
node scripts/image-qualification-control.mjs binding-probe-run \
  /private/evidence/plan.json /private/evidence/journal
```

The cycle is bounded to two Workers, one PUT per Worker, one authenticated runtime
request, 20 minutes total, forward stop at 10 minutes, and 800 request-budget units
(at most 400 before settlement). Each request has at most a 15-second timeout.
The USD 1 incremental ceiling is monitored by the operator, not a provider-enforced
billing cap. No retry, replacement, endpoint/format matrix, AWS allocation or
spending extension is implicit. An unexpected account size or denial can exhaust
the read budget; it does not justify skipping inventory.

Uploads may briefly inherit either provider public/preview default before private
normalization. Only these exact harmless modules admit that pending state.
After normalization the receiver stays private with previews disabled. The caller
is temporarily public with previews disabled, authenticates every invocation and
is deleted first. Public unauthenticated requests return 403; public traffic can
still consume provider request allowance. The operator must approve this bounded
exposure explicitly before execution.

## Evidence and recovery

`binding-probe.json` records requested nonsecret metadata, module digests, observed
immutable versions/uploader/default environment, settings bindings, exact-version
bindings, service-environment bindings and runtime props. Version/source checks
bracket management and runtime observation. A missing/denied alternate surface
makes the observation incomplete. Returned props can differ: that is experimental
data, never privileged ownership evidence. No result automatically changes the
qualification security contract or upload API.

Uploads and deletion intents are fsynced before dispatch. Cleanup rechecks the
module hash, first/single immutable version, uploader and creation window, exact
tags/capabilities, exposure, namespaces/domains and all incoming references.
It admits absent/changed props only on the service binding to this exact stateless
receiver, whose absence of resource bindings is independently proven. Ordinary
DELETE has no force parameter. Caller deletion must settle before receiver
references disappear. Settings 404 plus complete inventory verifies deletion.
An uncertain upload that was never observed owned remains UNKNOWN even if absent;
it cannot be marked settled or retried. A lost DELETE response can settle only
after identity was proven and subsequent absence independently observed.
Every recovery first journals pending settlement; a fresh failed check overrides
any earlier success.

After interruption, use the same frozen plan/source, existing credential custody
and existing durable journal. A missing journal or mistyped path refuses recovery;
it cannot initialize empty custody evidence or certify settlement. Recovery only inspects and ordinarily deletes exact owned
fixtures; it cannot create, invoke, replace or adopt another version:

```sh
node scripts/image-qualification-control.mjs binding-probe-recover \
  /private/evidence/plan.json /private/evidence/journal
```

Recovery has a separate five-minute command allowance after the original deadline;
it records whether settlement met the original cycle and never admits more forward
work. It is not a resource-lifetime extension or an automatic TTL. Cancellation
stops forward work while retaining settlement time and its management credential.
A second cancellation does not force termination of cleanup. On process death,
first prove exclusive same-host custody and that the lock's recorded PID has exited
before an operator removes only `binding-probe.lock`. There is no automatic stale
lock reclamation. Preserve the journal, source and recovery credential under any
unknown/foreign/denied state; do not force-delete or rotate away recovery access.

Local mocks test management-contract and lifecycle decisions, and local caller
checks test authentication. A local workerd check can establish local RPC/props
behavior for the exact modules; it cannot establish hosted upload or delivery.
These checks do not prove Cloudflare API schemas or live recovery. The separately approved live
fixture is the discriminator: retained runtime props plus an authoritative complete
management surface may justify a focused verifier correction; runtime props absent
instead selects a provider/upload investigation. Runtime-only delivery without an
authoritative readback requires a separately reviewed attestation contract. An
incomplete or version-unstable observation selects no correction.
