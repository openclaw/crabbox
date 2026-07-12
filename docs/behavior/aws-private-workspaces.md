# Private AWS Workspaces Behavior Contract

## User-Visible Goal

An authenticated API client can create, inspect, and delete one small private
Linux workspace through a dedicated Crabbox URL. Observable AWS evidence proves
that placement is limited to the configured account, Region, instance
allowlist, subnet, security group, encrypted volume, task-role identity, and
SSM-only bootstrap path.

## Target

- Type: HTTPS API plus operator-visible AWS resource and CloudWatch evidence.
- Launch or access: one deployed Node/PostgreSQL coordinator at the approved
  dedicated `WORKSPACE_API_URL`, from an approved network that can reach its
  internal load balancer; no shared coordinator may substitute.
- Allowed fixtures: one unique workspace ID, the request body below, and one
  separately approved negative-test ECS task definition when fail-closed
  startup probes are in scope.
- Credential sources: the exact environment variable
  `CRABBOX_RUNTIME_ADAPTER_TOKEN` from approved secret injection, and an
  approved AWS CLI identity with read-only evidence access. Never capture
  values, request headers, cookies, shell traces, or credential-provider output.
- Mutation gate: deployment, workspace creation/deletion, and negative task
  launches require a separate AWS GO. Without it, clauses that mutate AWS are
  `blocked_aws_go`, not pass or fail.

## User Tasks

1. Request `GET /v1/ready` at the dedicated origin and verify that the service
   reports ready only after its database and AWS preflight pass.
2. Verify that missing and incorrect bearer credentials cannot call workspace
   routes, then authenticate using the approved route-scoped bearer.
3. Create a unique workspace with `Idempotency-Key` equal to the workspace ID,
   an explicit long-running command, `ttlSeconds=1800`,
   `idleTimeoutSeconds=1800`, and desktop disabled.
4. Poll `GET /v1/workspaces/{id}` from `provisioning` to `ready`, and correlate
   the returned `providerResourceId` with exactly one EC2 instance.
5. Inspect the EC2 instance, root volume, subnet, route table, security group,
   IAM instance profile, tags, SSM managed-node state, SSM command result, and
   CloudWatch streams using operator-visible AWS APIs.
6. Repeat the identical create and verify it returns the same
   `providerResourceId`; repeat the ID with one immutable field changed and
   verify a conflict without a second EC2 instance.
7. Delete the workspace, poll to a terminal stopped state, verify the EC2
   instance terminates and its delete-on-termination root volume disappears,
   then repeat delete and verify safe terminal success.
8. In an isolated negative-test service, independently configure a wrong
   expected account, a wrong expected Region, missing ECS task metadata, and
   static AWS access keys. Each variant must remain not ready and must create no
   workspace resource.

## Expected Observable Behavior

- `/v1/health` can show liveness, but `/v1/ready` returns success only when
  PostgreSQL, ECS placement, STS identity, allowed types, private subnet,
  security groups, AMI resolution, the `RunInstances` permission dry-run, and
  SSM discovery are valid.
- Missing or wrong workspace bearer returns `401`; it does not fall through to
  a broader shared/admin identity and does not reveal configured identity or
  secret values.
- Valid create returns `202` with `status=provisioning`, a stable workspace ID,
  and a stable `providerResourceId`/`leaseId`. Invalid fields fail before EC2
  launch.
- `ready` is impossible until EC2 reports the instance, SSM reports the managed
  node `Online`, and the SSM bootstrap command reports `Success`.
- Private status exposes `provider=aws`, the lease ID, EC2
  `cloudResourceId`, exact Region and instance type, and SSM transport, command
  status, command ID, and log-group locator. These fields contain no secret.
- Exactly one instance exists in the expected account and Region. Its type is
  one of the server-configured x86_64 allowlist and does not exceed the
  configured vCPU or memory ceilings.
- The instance has no public IPv4 address, auto-assigned IPv6 address, EC2 key
  name, or ingress rule. It uses the configured private subnet, which has no
  direct internet-gateway default route.
- Instance metadata shows `HttpTokens=required`, hop limit `1`, endpoint
  enabled, and instance metadata tags disabled.
- For this contract's 20 GiB canary policy, the root volume is encrypted gp3 at
  exactly 20 GiB and has delete-on-termination enabled.
- The workspace security group is distinct from the controller group, has zero
  ingress rules, and every egress rule is TCP 443.
- Instance and volume tags contain `crabbox=true`, `created_by=crabbox`, the
  returned lease ID, `provider=aws`, `target=linux`, the selected
  `server_type`, `crabbox_workspace=true`, and `access_mode=ssm`. The remaining
  lease, owner, slug, timing, class, profile, market, keep, and state tags are
  present and internally consistent.
- The task uses temporary task-role credentials. The dedicated task definition
  contains no static access-key or secret-key value, and wrong-placement or
  static-key negative tasks fail before `RunInstances`.
- SSM and coordinator logs show create, bootstrap success, status transition,
  delete, and termination evidence. Responses and logs contain no bearer, AWS
  credential, database URL, signed request, or unrelated private data.
- Workspace ID, profile, repository, branch, and command contain no secret or
  credential material; these fields are durable and the command may appear in
  SSM history and retained systemd/log evidence.
- Identical create is idempotent. A changed immutable field for the same ID
  returns `409` and does not create another instance.
- Delete is idempotent. The first delete reaches `stopped`, the instance reaches
  `terminated`, its root volume disappears, and a repeated delete remains a
  successful stopped result. This live canary is also the permission proof for
  the tag-scoped `TerminateInstances` grant; startup cannot prove that condition
  against a nonexistent resource.
- A client-supplied target label, display label, or Region-shaped metadata
  value cannot change the AWS account, Region, subnet, security group, instance
  allowlist, or volume size. Placement changes only when the dedicated service
  configuration or URL changes.

## Anti-Cheat Probes

- Use a fresh workspace ID based on the current time; do not reuse a fixture
  whose result could have been pre-recorded.
- Observe at least one real `provisioning` response before `ready`, and correlate
  the returned lease ID to the live EC2 tag rather than trusting API text alone.
- Repeat the exact create, then alter only `command`; verify idempotency and
  conflict behavior against the live instance count.
- Call create, status, and delete once with no bearer and once with a deliberately
  incorrect bearer; verify no AWS inventory change.
- Query AWS independently for instance, volume, route, security-group, metadata,
  IAM, SSM, tag, log, and termination state.
- Refresh status after terminal deletion and repeat delete; do not accept a
  one-time success response as cleanup proof.
- Run each negative startup probe separately and compare EC2 inventory before
  and after. A generic error page without zero-resource proof is insufficient.

## Evidence Required

- Dedicated origin and redacted `/v1/ready` response.
- HTTP status and redacted response summaries for unauthenticated, invalid-auth,
  create, status transitions, identical create, conflicting create, first
  delete, post-delete status, and repeated delete.
- AWS account/Region identity summary without ARN or credential material.
- One redacted EC2 posture record covering type, private/public addressing,
  key name, subnet, security groups, metadata options, IAM profile, state, and
  tags.
- Redacted root-volume, route-table, and security-group summaries.
- Redacted SSM managed-node status, command ID/status, and CloudWatch log stream
  names; include only the minimal non-secret log excerpts needed to prove the
  lifecycle.
- EC2 termination and root-volume absence proof.
- For each negative startup probe: task outcome, readiness result, and
  before/after EC2 inventory count.

## Out Of Scope

- Any AWS or secret mutation before separate AWS GO.
- Normal Crabbox CLI SSH, rsync, terminal, desktop, browser, Code, VNC, or
  checkpoint behavior. Private workspace mode intentionally exposes none of
  those access paths.
- Application behavior inside the long-running workspace command beyond proof
  that SSM bootstrap started it successfully.
- Mutual-hostile-tenant isolation inside one trusted coordinator deployment.
- Static-key compatibility in existing non-private coordinator deployments.
- Source code, diffs, tests, build internals, or implementation notes. The
  validator must stay source-blind and use only the surfaces and evidence above.
