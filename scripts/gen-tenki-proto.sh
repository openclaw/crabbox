#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tools_dir="$(mktemp -d)"
trap 'rm -rf "$tools_dir"' EXIT
GOBIN="$tools_dir" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
cd "$root"
PATH="$tools_dir:$PATH" protoc --go_out=. --go_opt=module=github.com/openclaw/crabbox internal/providers/tenki/proto/ssh_gateway.proto
