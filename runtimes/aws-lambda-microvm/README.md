# Crabbox AWS Lambda MicroVM Runner

Application image used by `provider: aws-lambda-microvm`. The runner exposes
Lambda lifecycle hooks, bounded archive upload, health, and streamed command
execution on port 8080. AWS protects every public endpoint request with a
MicroVM-scoped JWE token; the runner accepts no independent public credential.

Package this directory as the code artifact when creating the MicroVM image:

```sh
cd runtimes/aws-lambda-microvm
zip -r /tmp/crabbox-lambda-microvm.zip Dockerfile *.go
aws s3 cp /tmp/crabbox-lambda-microvm.zip s3://<bucket>/crabbox-lambda-microvm.zip
```

See `docs/providers/aws-lambda-microvm.md` for the image-build and IAM commands.
