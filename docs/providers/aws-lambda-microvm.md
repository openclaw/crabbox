# AWS Lambda MicroVM Provider

Read when:

- choosing `provider: aws-lambda-microvm`;
- building the Crabbox runner image;
- changing `internal/providers/awslambdamicrovm` or its live smoke.

AWS Lambda MicroVM is a delegated Linux run provider. Crabbox launches an
isolated Firecracker MicroVM through the Lambda MicroVM API, uploads the
checkout as a portable archive, and streams commands through the bundled HTTP
runner. It does not expose SSH, VNC, browser/code access, or coordinator
brokering.

AWS documents Lambda MicroVMs as ARM64-only at launch, available in
`us-east-1`, `us-east-2`, `us-west-2`, `eu-west-1`, and `ap-northeast-1`, with
an eight-hour maximum lifetime. Check the
[AWS launch announcement](https://aws.amazon.com/about-aws/whats-new/2026/06/aws-lambda-microvms/)
and [developer guide](https://docs.aws.amazon.com/lambda/latest/dg/lambda-microvms-guide.html)
for current limits and Region availability.

## Build the runner image

The image source is `runtimes/aws-lambda-microvm`. Package its Dockerfile and
Go source, upload the artifact to S3, then create an image with the managed
Amazon Linux 2023 MicroVM base image:

```sh
cd runtimes/aws-lambda-microvm
zip -r /tmp/crabbox-lambda-microvm.zip Dockerfile *.go
aws s3 cp /tmp/crabbox-lambda-microvm.zip s3://my-bucket/crabbox-lambda-microvm.zip

aws lambda-microvms create-microvm-image \
  --name crabbox-runner \
  --code-artifact uri=s3://my-bucket/crabbox-lambda-microvm.zip \
  --base-image-arn arn:aws:lambda:eu-west-1:aws:microvm-image:al2023-1 \
  --build-role-arn arn:aws:iam::123456789012:role/MicrovmBuildRole
```

The build role must trust `lambda.amazonaws.com`, read the S3 artifact, and
write its CloudWatch build logs. AWS provides the exact trust and permissions
policies in [Create your first Lambda MicroVM](https://docs.aws.amazon.com/lambda/latest/dg/microvms-getting-started.html).
Wait until the image state is `CREATED` and its version is successful and
active before using it.

The local AWS identity used by Crabbox needs permission for `RunMicrovm`,
`GetMicrovm`, `ListMicrovms`, `CreateMicrovmAuthToken`, `SuspendMicrovm`,
`ResumeMicrovm`, and `TerminateMicrovm`. Add `iam:PassRole` only when using an
execution role.

## Config

```yaml
provider: aws-lambda-microvm
aws:
  region: eu-west-1
awsLambdaMicroVM:
  image: arn:aws:lambda:eu-west-1:123456789012:microvm-image:crabbox-runner
  imageVersion: "1.0"
  executionRoleArn: arn:aws:iam::123456789012:role/MicrovmRuntime
  workdir: /workspace/crabbox
  ingressConnectors: []
  egressConnectors: []
  forgetMissing: false
```

`image` is required. Empty connector lists use AWS-managed `ALL_INGRESS` and
`INTERNET_EGRESS` connectors. Set explicit connector ARNs to replace those
defaults. The image ARN and every connector must match `aws.region`.

Environment overrides:

```text
CRABBOX_AWS_LAMBDA_MICROVM_IMAGE
CRABBOX_AWS_LAMBDA_MICROVM_IMAGE_VERSION
CRABBOX_AWS_LAMBDA_MICROVM_EXECUTION_ROLE_ARN
CRABBOX_AWS_LAMBDA_MICROVM_WORKDIR
CRABBOX_AWS_LAMBDA_MICROVM_INGRESS_CONNECTORS
CRABBOX_AWS_LAMBDA_MICROVM_EGRESS_CONNECTORS
CRABBOX_AWS_LAMBDA_MICROVM_FORGET_MISSING
```

Matching command flags use the `--aws-lambda-microvm-` prefix. AWS credentials
and profiles use the standard AWS SDK credential chain; never put credentials
in Crabbox config or command arguments.

## Commands and lifecycle

```sh
crabbox doctor --provider aws-lambda-microvm --json
crabbox warmup --provider aws-lambda-microvm --slug lambda-test
crabbox run --provider aws-lambda-microvm -- go test ./...
crabbox run --provider aws-lambda-microvm --keep --slug lambda-test -- true
crabbox run --provider aws-lambda-microvm --id lambda-test --shell 'make test'
crabbox pause --provider aws-lambda-microvm lambda-test
crabbox resume --provider aws-lambda-microvm lambda-test
crabbox status --provider aws-lambda-microvm --id lambda-test --json
crabbox stop --provider aws-lambda-microvm lambda-test
crabbox cleanup --provider aws-lambda-microvm --dry-run
```

A fresh `run` is one-shot unless `--keep` or `--keep-on-failure` retains the
MicroVM. `warmup` always creates a retained lease. The provider sets Lambda's
maximum duration from Crabbox TTL, capped at eight hours, and maps the idle
timeout to Lambda auto-suspend/auto-resume. Explicit `pause` and `resume` call
the Lambda lifecycle APIs.

Each data-plane request gets a short-lived, port-8080-scoped JWE token through
`CreateMicrovmAuthToken`. Crabbox accepts only the Region-bound
`*.lambda-microvm.<region>.on.aws` endpoint returned by AWS and refuses
cross-origin redirects. See [Running and using MicroVMs](https://docs.aws.amazon.com/lambda/latest/dg/microvms-launching.html)
and [Networking](https://docs.aws.amazon.com/lambda/latest/dg/microvms-networking.html).

## Capabilities

- Target: Linux ARM64.
- Sync: portable archive upload and extraction.
- Command transport: streamed HTTP on port 8080.
- Retained reuse: yes, until explicit stop or Lambda lifetime expiry.
- Pause/resume and cleanup: yes.
- SSH, Actions hydration, checkpoints, desktop/browser/code/VNC: no.
- Coordinator: no; AWS SDK calls run directly from the CLI.

`doctor` is read-only: it loads AWS credentials, calls `ListMicrovms` for the
configured image/version, and reports the local claim count. It does not prove
that the custom runner is healthy; the first `warmup` or `run` performs that
check and rolls back an unhealthy MicroVM.

## Live smoke

The guarded smoke creates one retained MicroVM, proves archive sync and
streamed execution, reuses it, pauses and resumes it, then terminates it and
verifies local inventory cleanup:

```sh
CRABBOX_LIVE=1 \
CRABBOX_LIVE_PROVIDERS=aws-lambda-microvm \
CRABBOX_AWS_LAMBDA_MICROVM_IMAGE=arn:aws:lambda:eu-west-1:123456789012:microvm-image:crabbox-runner \
scripts/live-smoke.sh
```

The script always attempts `stop` after a lease is created. Missing credentials,
image configuration, Region availability, or quota produce an explicit blocked
classification rather than a false pass.
