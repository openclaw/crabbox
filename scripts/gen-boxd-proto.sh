#!/usr/bin/env bash
# Regenerate the committed boxd gRPC stubs from the vendored client subset at
# internal/providers/boxd/proto/api.proto. The subset's field numbers must
# match https://docs.boxd.sh/reference/grpc-proto exactly; see the proto
# header before editing. CI never runs this: generated code is committed.
set -euo pipefail
cd "$(dirname "$0")/.."
command -v protoc >/dev/null || { echo "protoc is required" >&2; exit 1; }
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
mkdir -p internal/providers/boxd/boxdapi
PATH="$(go env GOPATH)/bin:$PATH" protoc \
  --proto_path=internal/providers/boxd/proto \
  --go_out=internal/providers/boxd/boxdapi --go_opt=paths=source_relative \
  --go-grpc_out=internal/providers/boxd/boxdapi --go-grpc_opt=paths=source_relative \
  api.proto
gofmt -w internal/providers/boxd/boxdapi
