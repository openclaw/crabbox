# Private AWS Workspaces

Use this deployment when an authenticated service needs to create, inspect, and
delete small Linux workspaces in one dedicated AWS account and Region without
exposing SSH or a public instance address.

This is a specialized Node/PostgreSQL coordinator mode. It is separate from the
normal Crabbox CLI AWS path:

- normal `crabbox run --provider aws` leases an SSH-reachable machine and uses
  the class tables documented in [AWS](aws.md);
- the private workspace API uses an exact server-side instance-type allowlist,
  SSM bootstrap, and the route-scoped `/v1/workspaces` API;
- client labels, target IDs, or Region hints are metadata. They do not select
  the AWS account, Region, subnet, security group, instance type, or volume
  size. Only the dedicated coordinator configuration does that.

The current deployment supports the commercial `aws` partition only. It fails
closed in GovCloud, China, or other partitions whose endpoints and Canonical
Ubuntu publisher identity differ.

Do not point an isolation-sensitive client at a shared coordinator and treat a
client-side label as proof of placement. The client URL must resolve to this
dedicated service.

## Security and lifecycle contract

The supported private mode has these invariants:

- the Node coordinator resolves AWS credentials through the default provider
  chain; on ECS it uses the task role and refreshes temporary credentials;
- `CRABBOX_AWS_EXPECTED_ACCOUNT_ID` and
  `CRABBOX_AWS_EXPECTED_REGION` are both required and must match the live STS
  identity, ECS task metadata, and configured AWS endpoint;
- the ECS deployment rejects static `AWS_ACCESS_KEY_ID` and
  `AWS_SECRET_ACCESS_KEY` values instead of silently preferring them over the
  task role;
- allowed instance types are explicit, x86_64, and bounded by configured vCPU
  and memory ceilings. `t3a.small,t3.small`, two vCPUs, and 4096 MiB are the
  recommended small starting policy;
- the root disk is exactly the configured encrypted gp3 size and is deleted
  with the instance. The recommended default is 20 GiB;
- the workspace subnet must be available, must not auto-assign IPv4 or IPv6,
  and must not have a direct internet-gateway default route;
- the instance has no public IP, EC2 key pair, SSH ingress, or supported SSH
  access path;
- IMDS is enabled only with IMDSv2 tokens required, a hop limit of one, and
  instance metadata tags disabled;
- the workspace and controller use distinct security groups in the workspace
  VPC. The workspace group has no ingress, and every egress rule is TCP 443;
- the workspace instance profile supplies SSM managed-node access. Crabbox
  waits for SSM to report `Online`, sends bootstrap through
  `AWS-RunShellScript`, and does not report the workspace `ready` until the
  command succeeds;
- SSM command output goes to a stack-owned, retained workspace CloudWatch log
  group. A separate retained coordinator log group and durable workspace/lease
  status provide the other evidence planes;
- release verifies the exact provider resource and Crabbox ownership metadata,
  requests termination, and waits for the instance to disappear. Repeating a
  workspace delete is safe.

Private mode uses a stock x86_64 Ubuntu AMI with the SSM agent already present.
The bootstrap checks and starts that agent; it does not install or configure an
SSH server. Project-specific software remains the workspace command's
responsibility.

### Exact resource tags

Crabbox applies the same sanitized lease tag set to the on-demand instance and
root volume. The private-workspace tag keys are:

```text
Name
access_mode=ssm
class
crabbox=true
crabbox_workspace=true
created_at
created_by=crabbox
expires_at
idle_timeout_secs
keep=false
last_touched_at
lease
market
owner
profile
provider=aws
provider_key
server_type
slug
state=leased
target=linux
ttl_secs
```

Values that identify a lease or policy are derived by the coordinator and
sanitized before they reach AWS. Cleanup never treats tags alone as authority:
the durable lease binding and current provider resource must also match.

## Dedicated deployment owner

[`deploy/aws/ecs-fargate-coordinator.yaml`](../../deploy/aws/ecs-fargate-coordinator.yaml)
creates the deployment-owned control plane:

- one ECS Fargate cluster, task definition, and single-replica service;
- a task execution role that can read only the selected database and API-token
  secrets;
- a separate task role with the bounded EC2, SSM, STS, and `iam:PassRole`
  permissions needed for private workspaces;
- a workspace instance role/profile with SSM managed-instance access;
- separate controller, workspace, and load-balancer security groups;
- an internal HTTPS Application Load Balancer with an explicit client CIDR;
- retained coordinator and workspace-SSM CloudWatch log groups plus ECS
  Container Insights;
- deployment circuit-breaker rollback and an ALB readiness check on
  `/v1/ready`.

The template deliberately expects existing account-owned dependencies rather
than creating reusable shared infrastructure:

- one VPC, at least two internal load-balancer subnets in distinct Availability
  Zones, private ECS service subnets, and one private workspace subnet;
- route tables and DNS that let the task reach AWS APIs and PostgreSQL, and let
  workspaces reach SSM plus approved HTTPS bootstrap destinations;
- an ACM certificate and canonical HTTPS hostname reachable from the approved
  client network;
- a PostgreSQL 13+ database reachable through a TLS-verified `DATABASE_URL`;
- one Secrets Manager secret containing that URL and one containing the
  route-scoped workspace API token;
- an immutable coordinator image reference pinned by sha256 digest.

A private subnet can use NAT, a controlled HTTPS proxy, or appropriate VPC
endpoints. The startup preflight rejects a direct internet-gateway default
route; it does not manufacture missing SSM, Logs, repository, package, or DNS
reachability.

The execution role, coordinator task role, and workspace instance role are
intentionally different. Container image pull and secret injection belong to
the execution role. Workspace lifecycle belongs to the task role. SSM
managed-node access belongs to the workspace role. Do not add long-lived AWS
access keys to either Secrets Manager secret or to the task definition.
The stack explicitly denies Parameter Store reads on the workspace role so an
untrusted workspace command cannot reuse its instance credentials to retrieve
account parameters.

## Configuration

The CloudFormation template sets the private-mode environment. For a manual
Node deployment, configure the same contract:

| Setting | Contract |
| --- | --- |
| `CRABBOX_AWS_EXPECTED_ACCOUNT_ID` | Required exact 12-digit account ID. |
| `CRABBOX_AWS_EXPECTED_REGION` | Required exact Region. |
| `CRABBOX_AWS_EXPECTED_TASK_ROLE_NAME` | Required exact ECS coordinator task-role name; STS must report this assumed role. |
| `CRABBOX_AWS_REQUIRE_ECS_TASK=1` | Require ECS task metadata and task-role credentials. |
| `CRABBOX_WORKSPACE_PROVIDER=aws` | Route workspace lifecycle to AWS. |
| `CRABBOX_WORKSPACE_AWS_PRIVATE=1` | Enable SSM-only private mode. |
| `CRABBOX_WORKSPACE_AWS_INSTANCE_TYPES` | Required comma-separated exact allowlist; recommended `t3a.small,t3.small`. |
| `CRABBOX_WORKSPACE_AWS_MAX_VCPUS` | Maximum accepted vCPUs per allowed type; recommended `2`. |
| `CRABBOX_WORKSPACE_AWS_MAX_MEMORY_MIB` | Maximum accepted memory per allowed type; recommended `4096`. |
| `CRABBOX_WORKSPACE_AWS_ROOT_GB` | Encrypted gp3 root size, 8-100 GiB; recommended `20`. |
| `CRABBOX_WORKSPACE_AWS_SUBNET_ID` | Required private workspace subnet. |
| `CRABBOX_WORKSPACE_AWS_SECURITY_GROUP_ID` | Required no-ingress, TCP-443-egress workspace group. |
| `CRABBOX_WORKSPACE_AWS_CONTROLLER_SECURITY_GROUP_ID` | Required distinct controller group in the same VPC. |
| `CRABBOX_WORKSPACE_AWS_INSTANCE_PROFILE` | Required SSM instance profile name. |
| `CRABBOX_WORKSPACE_AWS_MARKET` | Required `on-demand` for this dedicated deployment. |
| `CRABBOX_WORKSPACE_AWS_SSM_LOG_GROUP` | Required CloudWatch log group for SSM command output. |
| `CRABBOX_RUNTIME_ADAPTER_TOKEN` | Required route-scoped bearer; inject from secret storage. |
| `CRABBOX_RUNTIME_ADAPTER_OWNER` | Stable service owner for all requests using that bearer. |
| `CRABBOX_RUNTIME_ADAPTER_ORG` | Stable organization boundary for all requests using that bearer. |
| `CRABBOX_MAX_ACTIVE_LEASES`, `CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER`, `CRABBOX_MAX_ACTIVE_LEASES_PER_ORG` | Required bounded concurrency; the template sets all three from `MaxActiveWorkspaces`. |
| `CRABBOX_MAX_MONTHLY_USD`, `CRABBOX_MAX_MONTHLY_USD_PER_OWNER`, `CRABBOX_MAX_MONTHLY_USD_PER_ORG` | Required cost-reservation ceilings; the template sets all three from `MaxMonthlyWorkspaceUSD`. |

`CRABBOX_WORKSPACE_SSH_PUBLIC_KEY` and
`CRABBOX_WORKSPACE_SSH_PRIVATE_KEY` are not used or required in private mode.
Static AWS keys remain a compatibility input for existing non-private
coordinator deployments, but are not required by the Node runtime and are
rejected by the dedicated ECS path.

## Startup preflight and readiness

Before the service becomes ready, the private deployment verifies:

1. ECS task metadata reports the expected account and Region from the exact
   Fargate metadata endpoint.
2. STS reports the expected AWS account and exact assumed task-role name.
3. Every allowlisted type exists, supports x86_64, and fits the vCPU and memory
   ceilings.
4. The subnet and route table satisfy the private-address rules.
5. The workspace and controller security groups satisfy the VPC, ingress, and
   TCP-443-egress rules.
6. The Ubuntu AMI resolves in the exact Region.
7. A `RunInstances` permission dry-run succeeds using the same private launch
   policy.
8. SSM managed-instance discovery is authorized.
9. PostgreSQL is reachable and ready.

Tag-scoped `TerminateInstances` authorization cannot be proved against a fake
resource: its IAM condition depends on tags on the live instance. The first
AWS-GO canary proves termination permission by deleting its real canary and
waiting for `terminated`.

`GET /v1/health` is liveness only. The load balancer and consumers must use
`GET /v1/ready`. A failed identity, policy, network, permission, or database
check keeps readiness closed and prevents workspace launch.

## Client API contract

The client needs exactly two deployment-scoped values:

- the canonical HTTPS origin configured as `PublicUrl`;
- the bearer stored server-side as `CRABBOX_RUNTIME_ADAPTER_TOKEN`.

Send the bearer as `Authorization: Bearer ...` to these routes:

| Request | Result |
| --- | --- |
| `POST /v1/workspaces` | Validate and reserve a workspace; returns `202` while provisioning or `200` when an identical workspace already exists. |
| `GET /v1/workspaces/{id}` | Return `provisioning`, `ready`, `stopping`, `stopped`, `expired`, or `failed`. |
| `DELETE /v1/workspaces/{id}` | Request generation-fenced cleanup; safe to retry. |

A private create should include an explicit long-running command:

```json
{
  "id": "service-canary-unique",
  "runtime": "crabbox",
  "profile": "private-linux",
  "command": "while :; do sleep 60; done",
  "ttlSeconds": 1800,
  "idleTimeoutSeconds": 1800,
  "capabilities": {
    "desktop": false
  }
}
```

Workspace IDs, profiles, repository/branch selectors, and commands are durable
request metadata. The command is also delivered through SSM and written into a
systemd start script, so it can appear in SSM history and retained operational
evidence. Never put passwords, bearer tokens, API keys, private repository
credentials, or other secrets in any of those fields. Workloads that need
credentials must retrieve them at runtime through the workspace instance role
and an approved secret source; project-specific secret delivery is outside this
deployment contract.

Use the workspace ID as the `Idempotency-Key` header. Repeating the same ID and
body returns the same `providerResourceId`; changing the immutable request for
that ID returns a conflict. A `ready` response means the EC2 instance is
running, SSM is online, and bootstrap succeeded. It does not mean SSH or a
public terminal is available.

Private AWS status responses also expose non-secret evidence fields:

```json
{
  "provider": "aws",
  "leaseId": "cbx_...",
  "providerResourceId": "cbx_...",
  "cloudResourceId": "i-...",
  "region": "us-west-2",
  "serverType": "t3a.small",
  "bootstrap": {
    "transport": "ssm",
    "status": "Success",
    "commandId": "...",
    "logGroup": "..."
  }
}
```

`providerResourceId` remains the compatibility name for the Crabbox lease ID;
`cloudResourceId` is the EC2 instance ID. SSM command and log identifiers are
evidence locators, not credentials.

The service URL selects the control plane. Client-side target labels, display
labels, or Region fields must not be interpreted as placement controls. To move
workloads to another account or Region, deploy another dedicated service and
change the client URL.

## Deployment procedure

Code review, merge, and a green build are not AWS deployment approval. Do not
run the following CloudFormation command, update secrets, or create a live
workspace until the deployment owner gives a separate AWS GO.

Before GO, prepare and review these non-secret values:

- exact account and Region;
- immutable image digest;
- VPC, load-balancer subnet, service subnet, and workspace subnet IDs;
- approved ingress and database CIDRs;
- certificate ARN and canonical URL;
- database URL secret ARN and route-token secret ARN;
- explicit instance allowlist, resource ceilings, and log retention. The
  dedicated stack fixes the market to on-demand; it does not depend on the
  account-level EC2 Spot service-linked role.
- concurrent-workspace and monthly cost-reservation ceilings. The monthly
  ceiling is a coordinator admission guard, not a substitute for an AWS Budget.

For on-demand workspaces, Crabbox does not treat spot-price history as an
on-demand quote. Without an explicit `CRABBOX_COST_RATES_JSON` override, cost
admission uses Crabbox's conservative generic AWS fallback. The deployment
defaults to at most two active workspaces and a $100 monthly reservation
ceiling; lower either parameter for a smaller budget.

After AWS GO, deploy from a clean, reviewed revision:

```bash
aws cloudformation deploy \
  --stack-name "$STACK_NAME" \
  --template-file deploy/aws/ecs-fargate-coordinator.yaml \
  --region "$EXPECTED_REGION" \
  --capabilities CAPABILITY_NAMED_IAM \
  --parameter-overrides \
    ExpectedAccountId="$EXPECTED_ACCOUNT_ID" \
    ExpectedRegion="$EXPECTED_REGION" \
    ImageUri="$IMAGE_URI" \
    PublicUrl="$WORKSPACE_API_URL" \
    VpcId="$VPC_ID" \
    LoadBalancerSubnetIds="$LOAD_BALANCER_SUBNET_IDS" \
    ServiceSubnetIds="$SERVICE_SUBNET_IDS" \
    IngressCidr="$INGRESS_CIDR" \
    CertificateArn="$CERTIFICATE_ARN" \
    DatabaseCidr="$DATABASE_CIDR" \
    DatabasePort="$DATABASE_PORT" \
    DatabaseUrlSecretArn="$DATABASE_URL_SECRET_ARN" \
    AuthTokenSecretArn="$WORKSPACE_TOKEN_SECRET_ARN" \
    RuntimeAdapterOwner="$RUNTIME_ADAPTER_OWNER" \
    RuntimeAdapterOrg="$RUNTIME_ADAPTER_ORG" \
    WorkspaceSubnetId="$WORKSPACE_SUBNET_ID" \
    AllowedWorkspaceInstanceTypes="$ALLOWED_WORKSPACE_INSTANCE_TYPES" \
    WorkspaceMaxVcpus=2 \
    WorkspaceMaxMemoryMiB=4096 \
    WorkspaceRootGiB="$WORKSPACE_ROOT_GIB" \
    MaxActiveWorkspaces=2 \
    MaxMonthlyWorkspaceUSD=100 \
    LogRetentionDays=30
```

Confirm the stack reaches `CREATE_COMPLETE` or `UPDATE_COMPLETE`, the ECS
service has one healthy task, and the canonical hostname reaches
`/v1/ready`. A failed deployment circuit breaker should roll the task back;
inspect the task stop reason and CloudWatch logs before retrying.

## Live canary runbook

This section creates and terminates paid AWS resources. Run it only after the
same separate AWS GO. Use an approved secret-injection tool to export
`CRABBOX_RUNTIME_ADAPTER_TOKEN`; never paste the value into a command, file,
ticket, or transcript. Run from the approved network that can reach the
internal load balancer, and disable shell tracing first. Run every block below
in the same Bash session; the first block installs an exit trap that attempts
workspace deletion if a later proof step fails.

```bash
set -e
set -o pipefail
set +x
test -n "${WORKSPACE_API_URL:-}"
test -n "${CRABBOX_RUNTIME_ADAPTER_TOKEN:-}"
test -n "${EXPECTED_ACCOUNT_ID:-}"
test -n "${EXPECTED_REGION:-}"
test -n "${STACK_NAME:-}"
test -n "${WORKSPACE_SUBNET_ID:-}"
test -n "${ALLOWED_WORKSPACE_INSTANCE_TYPES:-}"
test "${WORKSPACE_ROOT_GIB:-}" = 20

canary_dir="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-aws-canary.XXXXXX")"
chmod 700 "$canary_dir"
workspace_id="service-canary-$(date +%s)"

workspace_api() {
  method="$1"
  path="$2"
  shift 2
  printf 'Authorization: Bearer %s\n' "$CRABBOX_RUNTIME_ADAPTER_TOKEN" |
    curl --fail-with-body --silent --show-error \
      --header @- \
      --request "$method" \
      "${WORKSPACE_API_URL%/}${path}" \
      "$@"
}

workspace_created=0
cleanup_canary() {
  set +e
  if test "$workspace_created" = 1; then
    workspace_api DELETE "/v1/workspaces/$workspace_id" \
      >"$canary_dir/trap-delete.json" 2>/dev/null
  fi
  workspace_created=0
  unset CRABBOX_RUNTIME_ADAPTER_TOKEN
}
trap cleanup_canary EXIT
trap 'exit 130' INT TERM

curl --fail-with-body --silent --show-error \
  "${WORKSPACE_API_URL%/}/v1/ready" |
  tee "$canary_dir/ready.json" |
  jq .

jq -n \
  --arg id "$workspace_id" \
  '{
    id: $id,
    runtime: "crabbox",
    profile: "private-linux",
    command: "while :; do sleep 60; done",
    ttlSeconds: 1800,
    idleTimeoutSeconds: 1800,
    capabilities: {desktop: false}
  }' >"$canary_dir/create-request.json"

unauthenticated_status="$(curl --silent --show-error \
  --output "$canary_dir/unauthenticated.json" \
  --write-out '%{http_code}' \
  --request POST \
  --header 'content-type: application/json' \
  --data-binary "@$canary_dir/create-request.json" \
  "${WORKSPACE_API_URL%/}/v1/workspaces")"
test "$unauthenticated_status" = 401

wrong_token_status="$(printf 'Authorization: Bearer wrong\n' |
  curl --silent --show-error \
    --header @- \
    --output "$canary_dir/wrong-token.json" \
    --write-out '%{http_code}' \
    --request POST \
    --header 'content-type: application/json' \
    --data-binary "@$canary_dir/create-request.json" \
    "${WORKSPACE_API_URL%/}/v1/workspaces")"
test "$wrong_token_status" = 401

workspace_created=1
workspace_api POST /v1/workspaces \
  --header 'content-type: application/json' \
  --header "idempotency-key: $workspace_id" \
  --data-binary "@$canary_dir/create-request.json" |
  tee "$canary_dir/create-response.json" |
  jq .
```

Poll until the service reports `ready`; stop immediately on `failed`,
`expired`, or an unexpected state:

```bash
for attempt in $(seq 1 90); do
  workspace_api GET "/v1/workspaces/$workspace_id" >"$canary_dir/status.json"
  workspace_status="$(jq -r '.status' "$canary_dir/status.json")"
  case "$workspace_status" in
    ready) break ;;
    failed|expired|stopped) jq . "$canary_dir/status.json"; exit 1 ;;
    provisioning) sleep 10 ;;
    *) jq . "$canary_dir/status.json"; exit 1 ;;
  esac
done
test "$workspace_status" = ready

lease_id="$(jq -r '.providerResourceId' "$canary_dir/status.json")"
reported_instance_id="$(jq -r '.cloudResourceId' "$canary_dir/status.json")"
ssm_command_id="$(jq -r '.bootstrap.commandId' "$canary_dir/status.json")"
test "$(jq -r '.bootstrap.transport' "$canary_dir/status.json")" = ssm
test "$(jq -r '.bootstrap.status' "$canary_dir/status.json")" = Success
test "$(aws sts get-caller-identity --query Account --output text)" = "$EXPECTED_ACCOUNT_ID"

aws ec2 describe-instances \
  --region "$EXPECTED_REGION" \
  --filters \
    "Name=tag:lease,Values=$lease_id" \
    'Name=instance-state-name,Values=pending,running' \
  >"$canary_dir/instances.json"

test "$(jq '[.Reservations[].Instances[]] | length' "$canary_dir/instances.json")" -eq 1
instance_id="$(jq -r '.Reservations[0].Instances[0].InstanceId' "$canary_dir/instances.json")"
test "$instance_id" = "$reported_instance_id"
instance_type="$(jq -r '.Reservations[0].Instances[0].InstanceType' "$canary_dir/instances.json")"
subnet_id="$(jq -r '.Reservations[0].Instances[0].SubnetId' "$canary_dir/instances.json")"
vpc_id="$(jq -r '.Reservations[0].Instances[0].VpcId' "$canary_dir/instances.json")"
volume_id="$(jq -r '.Reservations[0].Instances[0].BlockDeviceMappings[0].Ebs.VolumeId' "$canary_dir/instances.json")"
security_group_id="$(jq -r '.Reservations[0].Instances[0].SecurityGroups[0].GroupId' "$canary_dir/instances.json")"
test "$subnet_id" = "$WORKSPACE_SUBNET_ID"
case ",$ALLOWED_WORKSPACE_INSTANCE_TYPES," in
  *",$instance_type,"*) ;;
  *) exit 1 ;;
esac

aws ec2 describe-instances \
  --region "$EXPECTED_REGION" \
  --instance-ids "$instance_id" \
  --query 'Reservations[0].Instances[0].{InstanceId:InstanceId,Type:InstanceType,PrivateIp:PrivateIpAddress,PublicIp:PublicIpAddress,Ipv6Addresses:NetworkInterfaces[].Ipv6Addresses[].Ipv6Address,KeyName:KeyName,SubnetId:SubnetId,SecurityGroups:SecurityGroups,Metadata:MetadataOptions,IamProfile:IamInstanceProfile,Tags:Tags}' \
  >"$canary_dir/instance-posture.json"
test "$(jq '[.Ipv6Addresses[]?] | length' "$canary_dir/instance-posture.json")" -eq 0
test -z "$(jq -r '.PublicIp // empty' "$canary_dir/instance-posture.json")"
test -z "$(jq -r '.KeyName // empty' "$canary_dir/instance-posture.json")"

aws ec2 describe-subnets \
  --region "$EXPECTED_REGION" \
  --subnet-ids "$subnet_id" \
  --query 'Subnets[0].{SubnetId:SubnetId,MapPublicIpOnLaunch:MapPublicIpOnLaunch,AssignIpv6AddressOnCreation:AssignIpv6AddressOnCreation}' \
  >"$canary_dir/subnet-posture.json"
test "$(jq -r '.MapPublicIpOnLaunch' "$canary_dir/subnet-posture.json")" = false
test "$(jq -r '.AssignIpv6AddressOnCreation' "$canary_dir/subnet-posture.json")" = false

aws ec2 describe-instance-types \
  --region "$EXPECTED_REGION" \
  --instance-types "$instance_type" \
  --query 'InstanceTypes[0].{Type:InstanceType,Architectures:ProcessorInfo.SupportedArchitectures,Vcpus:VCpuInfo.DefaultVCpus,MemoryMiB:MemoryInfo.SizeInMiB}' \
  >"$canary_dir/instance-type-posture.json"

aws ec2 describe-volumes \
  --region "$EXPECTED_REGION" \
  --volume-ids "$volume_id" \
  --query 'Volumes[0].{VolumeId:VolumeId,Type:VolumeType,Size:Size,Encrypted:Encrypted,Tags:Tags}' \
  >"$canary_dir/volume-posture.json"

aws ec2 describe-security-groups \
  --region "$EXPECTED_REGION" \
  --group-ids "$security_group_id" \
  --query 'SecurityGroups[0].{GroupId:GroupId,Ingress:IpPermissions,Egress:IpPermissionsEgress,Tags:Tags}' \
  >"$canary_dir/security-group-posture.json"

aws ec2 describe-route-tables \
  --region "$EXPECTED_REGION" \
  --filters "Name=vpc-id,Values=$vpc_id" \
  >"$canary_dir/route-tables.json"

aws ssm describe-instance-information \
  --region "$EXPECTED_REGION" \
  --filters "Key=InstanceIds,Values=$instance_id" \
  >"$canary_dir/ssm.json"
aws ssm get-command-invocation \
  --region "$EXPECTED_REGION" \
  --command-id "$ssm_command_id" \
  --instance-id "$instance_id" \
  >"$canary_dir/ssm-command.json"
test "$(jq -r '.Status' "$canary_dir/ssm-command.json")" = Success

coordinator_log_group="$(aws cloudformation describe-stack-resource \
  --stack-name "$STACK_NAME" \
  --logical-resource-id CoordinatorLogGroup \
  --region "$EXPECTED_REGION" \
  --query 'StackResourceDetail.PhysicalResourceId' \
  --output text)"
ssm_log_group="$(aws cloudformation describe-stack-resource \
  --stack-name "$STACK_NAME" \
  --logical-resource-id WorkspaceSSMLogGroup \
  --region "$EXPECTED_REGION" \
  --query 'StackResourceDetail.PhysicalResourceId' \
  --output text)"
aws logs describe-log-streams \
  --region "$EXPECTED_REGION" \
  --log-group-name "$coordinator_log_group" \
  --order-by LastEventTime \
  --descending \
  >"$canary_dir/coordinator-log-streams.json"
aws logs describe-log-streams \
  --region "$EXPECTED_REGION" \
  --log-group-name "$ssm_log_group" \
  --order-by LastEventTime \
  --descending \
  >"$canary_dir/ssm-log-streams.json"

ssm_stream="$(jq -r --arg instance "$instance_id" \
  '.logStreams[] | select(.logStreamName | contains($instance)) | .logStreamName' \
  "$canary_dir/ssm-log-streams.json" | head -n 1)"
test -n "$ssm_stream"
aws logs get-log-events \
  --region "$EXPECTED_REGION" \
  --log-group-name "$ssm_log_group" \
  --log-stream-name "$ssm_stream" \
  --limit 50 \
  >"$canary_dir/ssm-events.json"
aws logs filter-log-events \
  --region "$EXPECTED_REGION" \
  --log-group-name "$coordinator_log_group" \
  --filter-pattern "$lease_id" \
  --limit 50 \
  >"$canary_dir/coordinator-events.json"
```

The captured posture must show an allowlisted x86_64 type, no public address or
key name, IMDS tokens `required`, the configured private subnet and workspace
security group, a 20 GiB encrypted gp3 root volume, the exact ownership/access
tags above, and SSM `PingStatus=Online`. The security group must have no ingress
and only TCP 443 egress. Inspect the matching SSM command stream and coordinator
stream for successful bootstrap and lifecycle evidence; neither may contain the
bearer or AWS credentials.

Prove idempotency, then clean up:

```bash
workspace_api POST /v1/workspaces \
  --header 'content-type: application/json' \
  --header "idempotency-key: $workspace_id" \
  --data-binary "@$canary_dir/create-request.json" \
  >"$canary_dir/idempotent-create.json"
test "$(jq -r '.providerResourceId' "$canary_dir/idempotent-create.json")" = "$lease_id"

jq '.command = "while :; do sleep 120; done"' \
  "$canary_dir/create-request.json" \
  >"$canary_dir/conflicting-request.json"
conflict_status="$(printf 'Authorization: Bearer %s\n' "$CRABBOX_RUNTIME_ADAPTER_TOKEN" |
  curl --silent --show-error \
    --header @- \
    --output "$canary_dir/conflict.json" \
    --write-out '%{http_code}' \
    --request POST \
    --header 'content-type: application/json' \
    --header "idempotency-key: $workspace_id" \
    --data-binary "@$canary_dir/conflicting-request.json" \
    "${WORKSPACE_API_URL%/}/v1/workspaces")"
test "$conflict_status" = 409
test "$(aws ec2 describe-instances \
  --region "$EXPECTED_REGION" \
  --filters \
    "Name=tag:lease,Values=$lease_id" \
    'Name=instance-state-name,Values=pending,running' \
  --query 'length(Reservations[].Instances[])' \
  --output text)" = 1

workspace_api DELETE "/v1/workspaces/$workspace_id" \
  >"$canary_dir/delete.json"

for attempt in $(seq 1 90); do
  workspace_api GET "/v1/workspaces/$workspace_id" >"$canary_dir/delete-status.json"
  workspace_status="$(jq -r '.status' "$canary_dir/delete-status.json")"
  case "$workspace_status" in
    stopped) break ;;
    stopping) sleep 10 ;;
    *) jq . "$canary_dir/delete-status.json"; exit 1 ;;
  esac
done
test "$workspace_status" = stopped
aws ec2 wait instance-terminated \
  --region "$EXPECTED_REGION" \
  --instance-ids "$instance_id"

volume_absent=0
for attempt in $(seq 1 60); do
  aws ec2 describe-volumes \
    --region "$EXPECTED_REGION" \
    --filters "Name=volume-id,Values=$volume_id" \
    >"$canary_dir/volume-after-delete.json"
  if test "$(jq '.Volumes | length' "$canary_dir/volume-after-delete.json")" -eq 0; then
    volume_absent=1
    break
  fi
  sleep 5
done
test "$volume_absent" = 1

workspace_api DELETE "/v1/workspaces/$workspace_id" \
  >"$canary_dir/idempotent-delete.json"
test "$(jq -r '.status' "$canary_dir/idempotent-delete.json")" = stopped

for attempt in $(seq 1 30); do
  aws logs filter-log-events \
    --region "$EXPECTED_REGION" \
    --log-group-name "$coordinator_log_group" \
    --filter-pattern "$lease_id" \
    --limit 100 \
    >"$canary_dir/coordinator-events-final.json"
  jq -r \
    '.events[].message | fromjson? | select(.component == "crabbox_private_aws_workspace") | .event' \
    "$canary_dir/coordinator-events-final.json" | sort -u \
    >"$canary_dir/lifecycle-events.txt"
  if grep -qx create_accepted "$canary_dir/lifecycle-events.txt" &&
    grep -Eq '^(ready|recovered_ready)$' "$canary_dir/lifecycle-events.txt" &&
    grep -qx delete_requested "$canary_dir/lifecycle-events.txt" &&
    grep -qx terminated "$canary_dir/lifecycle-events.txt"; then
    break
  fi
  sleep 5
done
grep -qx create_accepted "$canary_dir/lifecycle-events.txt"
grep -Eq '^(ready|recovered_ready)$' "$canary_dir/lifecycle-events.txt"
grep -qx delete_requested "$canary_dir/lifecycle-events.txt"
grep -qx terminated "$canary_dir/lifecycle-events.txt"

workspace_created=0
trap - EXIT INT TERM
unset CRABBOX_RUNTIME_ADAPTER_TOKEN
```

Before removing `$canary_dir`, retain only approved redacted evidence: readiness,
workspace status transitions, instance/volume/security-group posture, SSM
status, log stream names, termination proof, and the repeated create/delete
results. Never retain request headers, shell traces, credential-provider output,
or secret values.

## Rollback and retirement

For an application rollback, redeploy the last reviewed image digest. The
PostgreSQL schema and workspace records remain deployment-owned; back up the
database before a version rollback.

For full retirement:

1. stop new callers by removing or disabling the dedicated client route;
2. list workspace records and wait for or explicitly delete every live
   workspace;
3. prove no running EC2 instance has the deployment's Crabbox ownership tags;
4. preserve approved lifecycle and SSM evidence for the required retention
   period;
5. scale the ECS service to zero and take the final PostgreSQL backup;
6. delete the CloudFormation stack; the retained coordinator and workspace-SSM
   log groups survive intentionally;
7. delete the retained log groups, dedicated secrets, database, certificate,
   routes, endpoints, and network resources only after their own retention and
   dependency checks.

Deleting the controller before workspaces are gone removes automatic cleanup
ownership. Do not use stack deletion as workspace cleanup.

## Related docs

- [AWS provider](../providers/aws.md)
- [Portable coordinator](portable-coordinator.md)
- [Infrastructure](../infrastructure.md)
- [Operations](../operations.md)
- [Operational security](../security.md)
- [Behavior contract](../behavior/aws-private-workspaces.md)
