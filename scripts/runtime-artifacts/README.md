# Runtime artifact manifests

This maintainer tool prepares and verifies local runtime manifests using the
same `internal/runtimeartifact` policy as the controller. It does not build,
download, execute, install, or modify input artifacts. It loads no Crabbox
configuration and needs no credentials. Archive extraction creates a separate
private staging directory.

Run from the repository root:

```sh
go run ./scripts/runtime-artifacts prepare \
  --directory /path/to/crabbox-runtime \
  --controller /path/to/crabbox

go run ./scripts/runtime-artifacts verify \
  --directory /path/to/crabbox-runtime \
  --controller /path/to/crabbox

go run ./scripts/runtime-artifacts extract \
  --archive /path/to/crabbox_X.Y.Z_darwin_arm64.tar.gz \
  --directory /path/to/new-stage --os darwin --arch arm64 \
  --runtime-pack final
```

`prepare` requires finalized `linux-amd64` and `linux-arm64` files in the named
directory. It prints the complete deterministic manifest as JSON on stdout.
The caller must publish those bytes atomically as `manifest.json` only after
all controller signing and runtime-file changes are complete. The manifest
binds the exact controller hash and both runtime hashes, sizes, and targets;
it does **not** establish their common-source provenance. The trusted producer
owns the frozen-source build and immutable handoff.

`verify` reads `manifest.json`, verifies its controller binding, and validates
both Linux runtime targets without executing them. Successful stdout is JSON
containing `protocolVersion` and ordered `artifacts` with `os`, `arch`, `size`,
and `sha256`. Archive membership and external release provenance remain the
release verifier's responsibility.

`extract` requires a nonexistent destination and an exact platform/architecture.
`none` selects the legacy CLI/Apple-helper inventory; `unsigned` adds both Linux
runtime files; `final` also requires a manifest bound to the archived controller.
Unexpected, duplicate, or nonregular payloads are rejected. Payload limits are
512 MiB per controller/Apple helper, 64 MiB per runtime, and 64 KiB per manifest.
Only an optional runtime-directory header is accepted in unsigned archives;
final archives list files explicitly. Files are staged with executable mode
0755, except the manifest (0644), and failed extraction removes its own directory.

Extraction JSON binds the archive basename, size, hash, OS and architecture to
the verified runtime/manifest identities. The caller must keep input archives
immutable and generate fresh reports through the protected tool. A report is
not self-authenticating proof of extraction. Release provenance combines these
reports with independent source/build metadata checks and pinned archive hashes.

All modes keep diagnostics on stderr. Exit status is 0 for success, 1 for
verification or I/O failure, and 2 for invalid arguments. `--help` works even
when other arguments are invalid. There are no prompts, configuration files,
environment overrides, or network operations in the compiled tool.

For release integration, compile the tool from the protected verifier source
in the credential-free preparation phase. Do not compile or execute candidate
source in a signing-credential environment. This tool alone does not change
the official archive inventory or authorize packaging, signing, or publication.
