import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const templatePath = path.resolve(scriptDirectory, "../deploy/aws/ecs-fargate-coordinator.yaml");
const template = await readFile(templatePath, "utf8");
const nodeDockerfile = await readFile(
  path.resolve(scriptDirectory, "../worker/Dockerfile.node"),
  "utf8",
);

function resourceBlock(name) {
  const resources = template.indexOf("\nResources:\n");
  assert.notEqual(resources, -1, "template must have Resources");
  const marker = `\n  ${name}:\n`;
  const start = template.indexOf(marker, resources);
  assert.notEqual(start, -1, `missing resource ${name}`);
  const bodyStart = start + marker.length;
  const nextResource = template.slice(bodyStart).search(/^  [A-Za-z][A-Za-z0-9]+:\n/gm);
  const end = nextResource === -1 ? template.length : bodyStart + nextResource;
  return template.slice(start, end);
}

function policyStatementBlock(role, sid) {
  const marker = `              - Sid: ${sid}\n`;
  const start = role.indexOf(marker);
  assert.notEqual(start, -1, `missing policy statement ${sid}`);
  const next = role.indexOf("\n              - Sid:", start + marker.length);
  return role.slice(start, next === -1 ? role.length : next);
}

function policyStatementBlocks(role) {
  const matches = [...role.matchAll(/^ {14}- Sid: ([A-Za-z0-9]+)\n/gm)];
  return matches.map((match, index) => ({
    sid: match[1],
    body: role.slice(match.index, matches[index + 1]?.index ?? role.length),
  }));
}

function assertAllMatches(text, expressions) {
  for (const expression of expressions) {
    assert.match(text, expression);
  }
}

function resourceNames() {
  const resourcesStart = template.indexOf("\nResources:\n");
  const outputsStart = template.indexOf("\nOutputs:\n", resourcesStart);
  assert.notEqual(resourcesStart, -1, "template must have Resources");
  assert.notEqual(outputsStart, -1, "template must have Outputs");
  return [
    ...template.slice(resourcesStart, outputsStart).matchAll(/^  ([A-Za-z][A-Za-z0-9]+):\n/gm),
  ].map((match) => match[1]);
}

test("Node coordinator image pins the AWS RDS trust bundle", () => {
  assert.match(
    nodeDockerfile,
    /ADD --checksum=sha256:e5bb2084ccf45087bda1c9bffdea0eb15ee67f0b91646106e466714f9de3c7e3 https:\/\/truststore\.pki\.rds\.amazonaws\.com\/global\/global-bundle\.pem \/etc\/ssl\/certs\/aws-rds-global-bundle\.pem/,
  );
  assert.match(
    nodeDockerfile,
    /NODE_EXTRA_CA_CERTS=\/etc\/ssl\/certs\/aws-rds-global-bundle\.pem/,
  );
});

test("Fargate coordinator template is generic and digest pinned", () => {
  assert.doesNotMatch(template, /fakeco|openclaw/i);
  assert.deepEqual(
    [...new Set(template.match(/\b[0-9]{12}\b/g) ?? [])],
    [],
    "the template must not contain deployment-specific account IDs",
  );
  assert.doesNotMatch(template, /AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN/);
  assert.match(template, /AllowedPattern: "\^\.\+@sha256:\[0-9a-f\]\{64\}\$"/);
  assert.match(template, /ExpectedAccountMatchesDeployment:/);
  assert.match(template, /ExpectedRegionMatchesDeployment:/);
  assert.match(template, /CoordinatorIngressIsBounded:/);
  assert.match(template, /DatabaseEgressIsBounded:/);
  assert.equal(
    template.match(/AllowedPattern: "\^\[0-9\.\]\+\/\(\[1-9\]\|\[12\]\[0-9\]\|3\[0-2\]\)\$"/g)
      ?.length,
    2,
  );
  assert.match(template, /RuntimeAdapterOrg:\n(?:.|\n)*MaxLength: 63/);
  assert.match(template, /AllowedPattern: '\^\[!-~\]/);
  assert.doesNotMatch(
    template,
    /LoadBalancerScheme|WorkspaceInstanceRoleArn:\n\s+Type:|WorkspaceInstanceProfileName:\n\s+Type:/,
  );
  assert.match(
    template,
    /AllowedWorkspaceInstanceTypes:\n\s+Type: CommaDelimitedList\n\s+Default: t3a\.small,t3\.small/,
  );
  assert.match(
    template,
    /WorkspaceRootGiB:\n\s+Type: Number\n\s+Default: 20\n\s+MinValue: 8\n\s+MaxValue: 100/,
  );
  assert.match(
    template,
    /MaxActiveWorkspaces:\n\s+Type: Number\n\s+Default: 2\n\s+MinValue: 1\n\s+MaxValue: 20/,
  );
  assert.match(
    template,
    /MaxMonthlyWorkspaceUSD:\n\s+Type: Number\n\s+Default: 100\n\s+MinValue: 1\n\s+MaxValue: 10000/,
  );

  const names = resourceNames();
  assert.equal(new Set(names).size, names.length, "resource logical IDs must be unique");
});

test("stack owns the single-replica Fargate and HTTPS ingress resources", () => {
  for (const [name, type] of [
    ["CoordinatorCluster", "AWS::ECS::Cluster"],
    ["CoordinatorTaskDefinition", "AWS::ECS::TaskDefinition"],
    ["CoordinatorService", "AWS::ECS::Service"],
    ["TaskExecutionRole", "AWS::IAM::Role"],
    ["CoordinatorTaskRole", "AWS::IAM::Role"],
    ["WorkspaceInstanceRole", "AWS::IAM::Role"],
    ["WorkspaceInstanceProfile", "AWS::IAM::InstanceProfile"],
    ["CoordinatorLoadBalancer", "AWS::ElasticLoadBalancingV2::LoadBalancer"],
    ["CoordinatorHttpsListener", "AWS::ElasticLoadBalancingV2::Listener"],
    ["CoordinatorTargetGroup", "AWS::ElasticLoadBalancingV2::TargetGroup"],
    ["CoordinatorLogGroup", "AWS::Logs::LogGroup"],
    ["WorkspaceSSMLogGroup", "AWS::Logs::LogGroup"],
  ]) {
    assert.match(resourceBlock(name), new RegExp(`Type: ${type.replaceAll("::", "\\:\\:")}`));
  }

  const service = resourceBlock("CoordinatorService");
  assert.match(service, /DesiredCount: 1/);
  assert.match(service, /MaximumPercent: 100/);
  assert.match(service, /MinimumHealthyPercent: 0/);
  assert.match(service, /AssignPublicIp: DISABLED/);
  assert.match(service, /LaunchType: FARGATE/);

  const target = resourceBlock("CoordinatorTargetGroup");
  assert.match(target, /HealthCheckPath: \/v1\/ready/);
  assert.match(target, /TargetType: ip/);

  for (const name of ["CoordinatorLogGroup", "WorkspaceSSMLogGroup"]) {
    const logGroup = resourceBlock(name);
    assert.match(logGroup, /DeletionPolicy: Retain/);
    assert.match(logGroup, /UpdateReplacePolicy: Retain/);
  }
});

test("task uses separate execution and task roles with injected secrets", () => {
  const task = resourceBlock("CoordinatorTaskDefinition");
  assert.match(task, /ExecutionRoleArn: !GetAtt TaskExecutionRole\.Arn/);
  assert.match(task, /TaskRoleArn: !GetAtt CoordinatorTaskRole\.Arn/);
  assert.match(task, /Name: DATABASE_URL\n\s+ValueFrom: !Ref DatabaseUrlSecretArn/);
  assert.match(task, /Name: CRABBOX_RUNTIME_ADAPTER_TOKEN\n\s+ValueFrom: !Ref AuthTokenSecretArn/);
  assert.match(task, /Name: CRABBOX_AWS_REQUIRE_ECS_TASK\n\s+Value: "1"/);
  assert.match(task, /Name: CRABBOX_AWS_EXPECTED_ACCOUNT_ID/);
  assert.match(task, /Name: CRABBOX_AWS_EXPECTED_REGION/);
  assert.match(
    task,
    /Name: CRABBOX_AWS_EXPECTED_TASK_ROLE_NAME\n\s+Value: !Ref CoordinatorTaskRole/,
  );
  assert.match(task, /Name: CRABBOX_WORKSPACE_AWS_PRIVATE\n\s+Value: "1"/);
  assert.match(task, /Name: CRABBOX_WORKSPACE_AWS_MARKET\n\s+Value: on-demand/);
  assert.match(task, /CRABBOX_WORKSPACE_AWS_INSTANCE_TYPES/);
  assert.match(task, /Name: CRABBOX_WORKSPACE_AWS_ROOT_GB\n\s+Value: !Ref WorkspaceRootGiB/);
  assert.match(task, /CRABBOX_WORKSPACE_AWS_SUBNET_ID/);
  assert.match(task, /CRABBOX_WORKSPACE_AWS_SECURITY_GROUP_ID/);
  assert.match(
    task,
    /Name: CRABBOX_WORKSPACE_AWS_INSTANCE_PROFILE\n\s+Value: !Ref WorkspaceInstanceProfile/,
  );
  assert.match(
    task,
    /Name: CRABBOX_WORKSPACE_AWS_SSM_LOG_GROUP\n\s+Value: !Ref WorkspaceSSMLogGroup/,
  );
  for (const name of [
    "CRABBOX_MAX_ACTIVE_LEASES",
    "CRABBOX_MAX_ACTIVE_LEASES_PER_OWNER",
    "CRABBOX_MAX_ACTIVE_LEASES_PER_ORG",
  ]) {
    assert.match(task, new RegExp(`Name: ${name}\\n\\s+Value: !Ref MaxActiveWorkspaces`));
  }
  for (const name of [
    "CRABBOX_MAX_MONTHLY_USD",
    "CRABBOX_MAX_MONTHLY_USD_PER_OWNER",
    "CRABBOX_MAX_MONTHLY_USD_PER_ORG",
  ]) {
    assert.match(task, new RegExp(`Name: ${name}\\n\\s+Value: !Ref MaxMonthlyWorkspaceUSD`));
  }
  assert.match(task, /StopTimeout: 120/);
  assert.match(task, /CRABBOX_SHUTDOWN_TIMEOUT_MS\n\s+Value: "100000"/);
});

test("stack-owned roles have scoped trust and workspace log access", () => {
  const executionRole = resourceBlock("TaskExecutionRole");
  const taskRole = resourceBlock("CoordinatorTaskRole");
  for (const role of [executionRole, taskRole]) {
    assertAllMatches(role, [
      /Service: ecs-tasks\.amazonaws\.com/,
      /aws:SourceAccount: !Ref ExpectedAccountId/,
      /aws:SourceArn: !Sub arn:\$\{AWS::Partition\}:ecs:\$\{ExpectedRegion\}:\$\{ExpectedAccountId\}:\*/,
    ]);
  }
  assert.equal(
    executionRole.match(/AmazonECSTaskExecutionRolePolicy/g)?.length,
    1,
    "execution managed policy must not be duplicated",
  );
  assertAllMatches(executionRole, [
    /Action: kms:Decrypt/,
    /- !Ref DatabaseUrlKmsKeyArn/,
    /- !Ref AuthTokenKmsKeyArn/,
    /kms:ViaService: !Sub secretsmanager\.\$\{ExpectedRegion\}\.\$\{AWS::URLSuffix\}/,
  ]);

  const workspaceRole = resourceBlock("WorkspaceInstanceRole");
  assertAllMatches(workspaceRole, [
    /Service: ec2\.amazonaws\.com/,
    /AmazonSSMManagedInstanceCore/,
    /PolicyName: DenyWorkspaceParameterReads/,
    /Sid: DenyParameterStoreReads/,
    /Effect: Deny/,
    /- ssm:GetParameter/,
    /- ssm:GetParameterHistory/,
    /- ssm:GetParameters/,
    /- ssm:GetParametersByPath/,
    /Action: logs:DescribeLogGroups/,
    /Action: logs:DescribeLogStreams/,
    /- logs:CreateLogStream/,
    /- logs:PutLogEvents/,
    /Resource: !Sub arn:\$\{AWS::Partition\}:logs:\$\{ExpectedRegion\}:\$\{ExpectedAccountId\}:log-group:\$\{WorkspaceSSMLogGroup\}/,
    /log-group:\$\{WorkspaceSSMLogGroup\}:log-stream:\*/,
  ]);
  assert.match(policyStatementBlock(workspaceRole, "DenyParameterStoreReads"), /Resource: "\*"/);
  assert.match(policyStatementBlock(workspaceRole, "DescribeWorkspaceLogGroups"), /Resource: "\*"/);
  assert.doesNotMatch(
    policyStatementBlock(workspaceRole, "DescribeWorkspaceLogStreams"),
    /Resource: "\*"/,
  );
  assert.doesNotMatch(
    policyStatementBlock(workspaceRole, "WriteWorkspaceCommandLogs"),
    /Resource: "\*"/,
  );
  assert.match(
    resourceBlock("WorkspaceInstanceProfile"),
    /Roles:\n\s+- !Ref WorkspaceInstanceRole/,
  );
});

test("workspace boundary is private, SSM-only, and IAM constrained", () => {
  const workspaceGroup = resourceBlock("WorkspaceSecurityGroup");
  assert.doesNotMatch(workspaceGroup, /SecurityGroupIngress:/);
  assert.equal(workspaceGroup.match(/IpProtocol:/g)?.length, 1);
  assert.match(workspaceGroup, /SecurityGroupEgress:\n(?:.|\n)*FromPort: 443\n\s+ToPort: 443/);
  assert.doesNotMatch(workspaceGroup, /FromPort: 22|ToPort: 22/);

  const role = resourceBlock("CoordinatorTaskRole");
  assert.match(role, /ec2:DescribeRouteTables/);
  assert.match(role, /Sid: DenyPublicWorkspaceAddress/);

  const runInstanceAllows = policyStatementBlocks(role).filter(
    ({ body }) => /Effect: Allow/.test(body) && /Action: ec2:RunInstances/.test(body),
  );
  assert.deepEqual(
    runInstanceAllows.map(({ sid }) => sid),
    [
      "LaunchFromCanonicalUbuntuImage",
      "LaunchWorkspaceInstance",
      "LaunchWorkspaceVolume",
      "LaunchWorkspaceNetworkInterface",
      "UseWorkspaceSubnetForLaunch",
      "UseWorkspaceSecurityGroupForLaunch",
    ],
  );
  for (const { body } of runInstanceAllows) {
    assert.doesNotMatch(body, /Resource: "\*"/);
  }
  const ec2ConditionKeys = [
    ...new Set(
      runInstanceAllows.flatMap(({ body }) =>
        [...body.matchAll(/^\s+(ec2:[A-Za-z0-9]+):/gm)].map((match) => match[1]),
      ),
    ),
  ].sort();
  assert.deepEqual(ec2ConditionKeys, [
    "ec2:AssociatePublicIpAddress",
    "ec2:Encrypted",
    "ec2:InstanceProfile",
    "ec2:InstanceType",
    "ec2:MetadataHttpTokens",
    "ec2:Owner",
    "ec2:Subnet",
    "ec2:VolumeSize",
    "ec2:VolumeType",
  ]);
  assert.doesNotMatch(role, /Sid: LaunchBoundedPrivateWorkspace/);
  assert.doesNotMatch(role, /spot-instances-request|DescribeSpotPriceHistory/);

  const image = policyStatementBlock(role, "LaunchFromCanonicalUbuntuImage");
  assertAllMatches(image, [
    /Resource: !Sub arn:\$\{AWS::Partition\}:ec2:\$\{ExpectedRegion\}::image\/ami-\*/,
    /ec2:Owner: amazon/,
  ]);

  const instance = policyStatementBlock(role, "LaunchWorkspaceInstance");
  assertAllMatches(instance, [
    /Resource: !Sub arn:\$\{AWS::Partition\}:ec2:\$\{ExpectedRegion\}:\$\{ExpectedAccountId\}:instance\/\*/,
    /ec2:InstanceType: !Ref AllowedWorkspaceInstanceTypes/,
    /ec2:MetadataHttpTokens: required/,
    /ec2:InstanceProfile: !GetAtt WorkspaceInstanceProfile\.Arn/,
    /"aws:RequestTag\/crabbox": "true"/,
    /"aws:RequestTag\/created_by": "crabbox"/,
    /"aws:RequestTag\/crabbox_workspace": "true"/,
    /"aws:RequestTag\/access_mode": "ssm"/,
  ]);
  assert.doesNotMatch(instance, /ec2:Volume|ec2:Encrypted|ec2:Subnet/);

  const volume = policyStatementBlock(role, "LaunchWorkspaceVolume");
  assertAllMatches(volume, [
    /Resource: !Sub arn:\$\{AWS::Partition\}:ec2:\$\{ExpectedRegion\}:\$\{ExpectedAccountId\}:volume\/\*/,
    /ec2:VolumeType: gp3/,
    /ec2:Encrypted: "true"/,
    /NumericEquals:\n\s+ec2:VolumeSize: !Ref WorkspaceRootGiB/,
    /"aws:RequestTag\/crabbox": "true"/,
    /"aws:RequestTag\/created_by": "crabbox"/,
    /"aws:RequestTag\/crabbox_workspace": "true"/,
    /"aws:RequestTag\/access_mode": "ssm"/,
  ]);
  assert.doesNotMatch(volume, /ec2:InstanceType|ec2:MetadataHttpTokens|ec2:Subnet/);

  const networkInterface = policyStatementBlock(role, "LaunchWorkspaceNetworkInterface");
  assertAllMatches(networkInterface, [
    /Resource: !Sub arn:\$\{AWS::Partition\}:ec2:\$\{ExpectedRegion\}:\$\{ExpectedAccountId\}:network-interface\/\*/,
    /ec2:Subnet: !Sub arn:\$\{AWS::Partition\}:ec2:\$\{ExpectedRegion\}:\$\{ExpectedAccountId\}:subnet\/\$\{WorkspaceSubnetId\}/,
    /ec2:AssociatePublicIpAddress: "false"/,
  ]);
  assert.doesNotMatch(networkInterface, /aws:RequestTag|ec2:Volume|ec2:InstanceType/);

  assert.match(
    policyStatementBlock(role, "UseWorkspaceSubnetForLaunch"),
    /Resource: !Sub arn:\$\{AWS::Partition\}:ec2:\$\{ExpectedRegion\}:\$\{ExpectedAccountId\}:subnet\/\$\{WorkspaceSubnetId\}/,
  );
  assert.match(
    policyStatementBlock(role, "UseWorkspaceSecurityGroupForLaunch"),
    /Resource: !Sub arn:\$\{AWS::Partition\}:ec2:\$\{ExpectedRegion\}:\$\{ExpectedAccountId\}:security-group\/\$\{WorkspaceSecurityGroup\}/,
  );
  assert.doesNotMatch(role, /subnet\/\*|security-group\/\*|key-pair\/|KeyPair/);

  const launchTags = policyStatementBlock(role, "TagWorkspaceAtLaunch");
  assertAllMatches(launchTags, [
    /instance\/\*/,
    /volume\/\*/,
    /"aws:RequestTag\/crabbox": "true"/,
    /"aws:RequestTag\/created_by": "crabbox"/,
    /"aws:RequestTag\/crabbox_workspace": "true"/,
    /"aws:RequestTag\/access_mode": "ssm"/,
  ]);
  assert.doesNotMatch(launchTags, /Resource: "\*"|network-interface\/\*/);

  const terminate = policyStatementBlock(role, "TerminateOwnedWorkspace");
  assertAllMatches(terminate, [
    /"ec2:ResourceTag\/crabbox": "true"/,
    /"ec2:ResourceTag\/created_by": "crabbox"/,
    /"ec2:ResourceTag\/crabbox_workspace": "true"/,
    /"ec2:ResourceTag\/access_mode": "ssm"/,
  ]);

  const document = policyStatementBlock(role, "AllowWorkspaceCommandDocument");
  assert.match(document, /document\/AWS-RunShellScript/);
  assert.doesNotMatch(document, /instance\/\*/);
  const send = policyStatementBlock(role, "SendCommandToOwnedWorkspace");
  assertAllMatches(send, [
    /Action: ssm:SendCommand/,
    /instance\/\*/,
    /"ssm:resourceTag\/crabbox": "true"/,
    /"ssm:resourceTag\/created_by": "crabbox"/,
    /"ssm:resourceTag\/crabbox_workspace": "true"/,
    /"ssm:resourceTag\/access_mode": "ssm"/,
  ]);
  assert.doesNotMatch(send, /document\/AWS-RunShellScript/);
  assert.match(role, /ssm:GetCommandInvocation/);
  assert.match(role, /ssm:DescribeInstanceInformation/);
  assert.doesNotMatch(role, /StringEqualsIfExists|ssm:CancelCommand|ssm:ListCommandInvocations/);
  assert.match(
    policyStatementBlock(role, "PassWorkspaceInstanceRole"),
    /Resource: !GetAtt WorkspaceInstanceRole\.Arn/,
  );
  assert.doesNotMatch(role, /ImportKeyPair|AuthorizeSecurityGroupIngress|WorkspaceInstanceRoleArn/);
});

test("load balancer reaches only the coordinator and liveness stays separate", () => {
  const loadBalancer = resourceBlock("CoordinatorLoadBalancer");
  assert.match(loadBalancer, /Scheme: internal/);
  assert.match(
    loadBalancer,
    /Key: idle_timeout\.timeout_seconds\n\s+Value: "180"/,
  );
  assert.doesNotMatch(loadBalancer, /internet-facing|LoadBalancerScheme/);

  const loadBalancerGroup = resourceBlock("LoadBalancerSecurityGroup");
  assert.match(loadBalancerGroup, /CidrIp: !Ref IngressCidr/);
  assert.match(loadBalancerGroup, /DestinationSecurityGroupId: !Ref CoordinatorSecurityGroup/);

  const ingress = resourceBlock("CoordinatorIngressFromLoadBalancer");
  assert.match(ingress, /SourceSecurityGroupId: !Ref LoadBalancerSecurityGroup/);
  assert.match(ingress, /FromPort: 8080/);

  const task = resourceBlock("CoordinatorTaskDefinition");
  assert.match(task, /127\.0\.0\.1:8080\/v1\/health/);
  assert.doesNotMatch(task, /127\.0\.0\.1:8080\/v1\/ready/);

  const environmentNames = [...task.matchAll(/- Name: ([A-Z][A-Z0-9_]+)/g)].map(
    (match) => match[1],
  );
  assert.equal(
    new Set(environmentNames).size,
    environmentNames.length,
    "container environment names must be unique",
  );
});
