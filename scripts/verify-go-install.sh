#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
MODULE=github.com/openclaw/crabbox
FORK_VERSION=v6.0.3-0.20260817142523-966654abed4a
VERSION=${1:-}
SOURCE_REF=${2:-}

fail() {
  echo "go install verification failed: $*" >&2
  exit 1
}

if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ || -z "$SOURCE_REF" ]]; then
  echo "usage: $0 vX.Y.Z <exact-source-ref>" >&2
  exit 2
fi
for tool in git go tar zip; do
  command -v "$tool" >/dev/null || fail "missing required tool: $tool"
done

SOURCE_COMMIT=$(git -C "$ROOT" rev-parse --verify "${SOURCE_REF}^{commit}")
[[ "$SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]] || fail "source ref did not resolve to a commit"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/crabbox-go-install.XXXXXX")
cleanup() {
  chmod -R u+w "$WORK" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

SOURCE="$WORK/source"
PROXY="$WORK/proxy"
SEED_HOME="$WORK/seed-home"
SEED_MODCACHE="$WORK/seed-modcache"
SEED_GOCACHE="$WORK/seed-gocache"
SEED_TMP="$WORK/seed-tmp"
mkdir -m 700 "$SOURCE" "$PROXY" "$SEED_HOME" "$SEED_MODCACHE" "$SEED_GOCACHE" "$SEED_TMP"
git -C "$ROOT" archive "$SOURCE_COMMIT" | tar -xf - -C "$SOURCE"

VERIFY_GO="$WORK/verify.go"
cat >"$VERIFY_GO" <<'EOF'
package main

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	modulePath  = "github.com/openclaw/crabbox"
	forkPath    = "github.com/steipete/jsonschema/v6"
	upstreamPath = "github.com/santhosh-tekuri/jsonschema/v6"
)

type module struct {
	Path    string  `json:"Path"`
	Version string  `json:"Version"`
	Replace *module `json:"Replace"`
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func decode(path string, value any) {
	data, err := os.ReadFile(path)
	if err != nil {
		fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		fatalf("decode %s: %v", path, err)
	}
}

func verifySource(modJSON, sourceRoot, forkVersion string) {
	var edit struct {
		Module  module
		Require []struct {
			Path     string
			Version  string
			Indirect bool
		}
		Replace []json.RawMessage
	}
	decode(modJSON, &edit)
	if edit.Module.Path != modulePath {
		fatalf("module path=%q want %q", edit.Module.Path, modulePath)
	}
	if len(edit.Replace) != 0 {
		fatalf("go.mod contains replacement directives")
	}
	forkRequires := 0
	for _, requirement := range edit.Require {
		switch requirement.Path {
		case forkPath:
			forkRequires++
			if requirement.Version != forkVersion || requirement.Indirect {
				fatalf("fork requirement=%s indirect=%t want %s direct", requirement.Version, requirement.Indirect, forkVersion)
			}
		case upstreamPath:
			fatalf("go.mod still requires upstream JSON Schema module")
		}
	}
	if forkRequires != 1 {
		fatalf("direct fork requirement count=%d want 1", forkRequires)
	}

	forkImports := 0
	err := filepath.Walk(sourceRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "vendor" || info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			value, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			switch value {
			case forkPath:
				forkImports++
			case upstreamPath:
				fatalf("source still imports upstream JSON Schema module: %s", path)
			}
		}
		return nil
	})
	if err != nil {
		fatalf("inspect source imports: %v", err)
	}
	if forkImports == 0 {
		fatalf("source does not directly import the JSON Schema fork")
	}
}

func verifyBinary(metadataPath, version, forkVersion string) {
	var info struct {
		Path string
		Main module
		Deps []module
	}
	decode(metadataPath, &info)
	if info.Path != modulePath+"/cmd/crabbox" {
		fatalf("binary package=%q want %q", info.Path, modulePath+"/cmd/crabbox")
	}
	if info.Main.Path != modulePath || info.Main.Version != version || info.Main.Replace != nil {
		fatalf("binary main=%s@%s replacement=%t want %s@%s without replacement", info.Main.Path, info.Main.Version, info.Main.Replace != nil, modulePath, version)
	}
	forkDeps := 0
	for _, dependency := range info.Deps {
		if dependency.Replace != nil {
			fatalf("binary dependency %s contains a replacement record", dependency.Path)
		}
		switch dependency.Path {
		case forkPath:
			forkDeps++
			if dependency.Version != forkVersion {
				fatalf("binary fork dependency=%s want %s", dependency.Version, forkVersion)
			}
		case upstreamPath:
			fatalf("binary contains upstream JSON Schema dependency")
		}
	}
	if forkDeps != 1 {
		fatalf("binary fork dependency count=%d want 1", forkDeps)
	}
}

func sanitizeNestedModules(root string) {
	root = filepath.Clean(root)
	rootGoMod := filepath.Join(root, "go.mod")
	var nested []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && info.Name() == "go.mod" && path != rootGoMod {
			nested = append(nested, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		fatalf("inspect nested modules: %v", err)
	}
	sort.Slice(nested, func(i, j int) bool {
		return len(nested[i]) > len(nested[j])
	})
	for _, dir := range nested {
		if err := os.RemoveAll(dir); err != nil {
			fatalf("remove nested module %q: %v", dir, err)
		}
	}
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && info.Name() == "go.mod" && path != rootGoMod {
			return fmt.Errorf("nested module remains: %q", path)
		}
		return nil
	})
	if err != nil {
		fatalf("verify nested module cleanup: %v", err)
	}
}

func main() {
	if len(os.Args) < 2 {
		fatalf("missing verifier action")
	}
	switch os.Args[1] {
	case "source":
		if len(os.Args) != 5 {
			fatalf("source verifier expects mod-json, source-root, and fork-version")
		}
		verifySource(os.Args[2], os.Args[3], os.Args[4])
	case "binary":
		if len(os.Args) != 5 {
			fatalf("binary verifier expects metadata, version, and fork-version")
		}
		verifyBinary(os.Args[2], os.Args[3], os.Args[4])
	case "sanitize-zip":
		if len(os.Args) != 3 {
			fatalf("zip sanitizer expects a module root")
		}
		sanitizeNestedModules(os.Args[2])
	default:
		fatalf("unknown verifier action %q", os.Args[1])
	}
}

EOF

MOD_JSON="$WORK/go-mod.json"
(
  cd "$SOURCE"
  env -i \
    GOENV=off GOTOOLCHAIN=local GOWORK=off \
    HOME="$SEED_HOME" PATH="$PATH" TMPDIR="$SEED_TMP" \
    go mod edit -json >"$MOD_JSON"
)
env -i \
  GOCACHE="$SEED_GOCACHE" GOENV=off GOTOOLCHAIN=local GOWORK=off \
  HOME="$SEED_HOME" PATH="$PATH" TMPDIR="$SEED_TMP" \
  go run "$VERIFY_GO" source "$MOD_JSON" "$SOURCE" "$FORK_VERSION"

# Download the complete selected dependency graph into an isolated cache, then
# copy the proxy-form cache entries. Network access ends before the install.
(
  cd "$SOURCE"
  env -i \
    GOCACHE="$SEED_GOCACHE" GOMODCACHE="$SEED_MODCACHE" \
    GOENV=off GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org \
    GOTOOLCHAIN=local GOWORK=off \
    HOME="$SEED_HOME" PATH="$PATH" TMPDIR="$SEED_TMP" \
    go mod download all
)
[[ -d "$SEED_MODCACHE/cache/download" ]] || fail "dependency download did not create proxy cache metadata"
cp -R "$SEED_MODCACHE/cache/download/." "$PROXY/"

MODULE_PROXY="$PROXY/github.com/openclaw/crabbox/@v"
mkdir -p "$MODULE_PROXY"
cp "$SOURCE/go.mod" "$MODULE_PROXY/$VERSION.mod"
COMMIT_TIME=$(git -C "$ROOT" show -s --format=%cI "$SOURCE_COMMIT")
printf '{"Version":"%s","Time":"%s"}\n' "$VERSION" "$COMMIT_TIME" >"$MODULE_PROXY/$VERSION.info"
printf '%s\n' "$VERSION" >"$MODULE_PROXY/list"

# A proxy module zip excludes nested modules. Stage the tracked source under
# the required module@version prefix and remove each nested module subtree.
ZIP_ROOT="$WORK/module-zip"
ZIP_SOURCE="$ZIP_ROOT/$MODULE@$VERSION"
mkdir -p "$(dirname "$ZIP_SOURCE")"
cp -R "$SOURCE" "$ZIP_SOURCE"
env -i \
  GOCACHE="$SEED_GOCACHE" GOENV=off GOTOOLCHAIN=local GOWORK=off \
  HOME="$SEED_HOME" PATH="$PATH" TMPDIR="$SEED_TMP" \
  go run "$VERIFY_GO" sanitize-zip "$ZIP_SOURCE"
(
  cd "$ZIP_ROOT"
  zip -q -X -r "$MODULE_PROXY/$VERSION.zip" "$MODULE@$VERSION"
)
chmod -R a-w "$PROXY"

INSTALL_ROOT="$WORK/install"
INSTALL_HOME="$INSTALL_ROOT/home"
INSTALL_GOPATH="$INSTALL_ROOT/gopath"
INSTALL_MODCACHE="$INSTALL_ROOT/gomodcache"
INSTALL_GOCACHE="$INSTALL_ROOT/gocache"
INSTALL_GOBIN="$INSTALL_ROOT/bin"
INSTALL_TMP="$INSTALL_ROOT/tmp"
INSTALL_CWD="$INSTALL_ROOT/work"
mkdir -m 700 "$INSTALL_ROOT"
mkdir -m 700 \
  "$INSTALL_HOME" "$INSTALL_GOPATH" "$INSTALL_MODCACHE" "$INSTALL_GOCACHE" \
  "$INSTALL_GOBIN" "$INSTALL_TMP" "$INSTALL_CWD"

(
  cd "$INSTALL_CWD"
  env -i \
    GOBIN="$INSTALL_GOBIN" GOCACHE="$INSTALL_GOCACHE" \
    GOENV=off GOMODCACHE="$INSTALL_MODCACHE" GOPATH="$INSTALL_GOPATH" \
    GOPROXY="file://$PROXY" GOSUMDB=off GOTOOLCHAIN=local GOWORK=off \
    HOME="$INSTALL_HOME" PATH="$PATH" TMPDIR="$INSTALL_TMP" \
    go install "$MODULE/cmd/crabbox@$VERSION"
)

BINARY="$INSTALL_GOBIN/crabbox"
[[ -x "$BINARY" ]] || fail "go install did not produce an executable"
METADATA="$WORK/build-metadata.json"
env -i \
  GOENV=off GOTOOLCHAIN=local HOME="$INSTALL_HOME" PATH="$PATH" TMPDIR="$INSTALL_TMP" \
  go version -m -json "$BINARY" >"$METADATA"
env -i \
  GOCACHE="$SEED_GOCACHE" GOENV=off GOTOOLCHAIN=local GOWORK=off \
  HOME="$SEED_HOME" PATH="$PATH" TMPDIR="$SEED_TMP" \
  go run "$VERIFY_GO" binary "$METADATA" "$VERSION" "$FORK_VERSION"

EXPECTED_VERSION=${VERSION#v}
ACTUAL_VERSION=$(cd "$INSTALL_CWD" && env -i HOME="$INSTALL_HOME" PATH="$PATH" TMPDIR="$INSTALL_TMP" "$BINARY" --version)
[[ "$ACTUAL_VERSION" == "$EXPECTED_VERSION" ]] || fail "crabbox --version=$ACTUAL_VERSION want $EXPECTED_VERSION"
(
  cd "$INSTALL_CWD"
  env -i HOME="$INSTALL_HOME" PATH="$PATH" TMPDIR="$INSTALL_TMP" "$BINARY" --help >/dev/null 2>&1
  env -i HOME="$INSTALL_HOME" PATH="$PATH" TMPDIR="$INSTALL_TMP" "$BINARY" run --help >/dev/null 2>&1
)

echo "Verified remote go install: $MODULE/cmd/crabbox@$VERSION source=$SOURCE_COMMIT"
