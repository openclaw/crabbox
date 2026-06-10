# WASI Provider (experimental, specialized)

Read when:

- choosing `provider: wasi` (aliases `wasm`, `wazero`);
- running WebAssembly modules or Wasm-first workloads with capability-based isolation;
- using `crabbox run` with lightweight `.wasm` artifacts;
- evaluating generated or untrusted code that can be compiled to WASI.

WASI is an **experimental delegated-run provider**. It executes `.wasm` (wasm32-wasi / preview1 or preview2) modules inside an embedded wazero runtime (or wasmtime if configured and available). The CLI owns local claims + slugs + an isolated guest root; "sync" copies the manifest into the guest tree and preopens it for the module. There is no SSH, no broker, and no full Linux surface.

See the official [WASI site](https://wasi.dev/) and [WASI specification](https://github.com/WebAssembly/WASI) for the API family, proposals, and versioning. This provider targets Preview 1 / WASI 0.1 for broad toolchain compatibility today, with wasmtime available for runtimes that support newer component-model work.

wazero and wasmtime are both listed by wasi.dev. This provider follows the WASI capability model by granting only the synced guest tree as `/work`, plus stdio, args, and explicit environment variables. Convenience builtins such as `ls` and `cat` use the same `/work` boundary.

It is deliberately specialized for:
- Projects that cross-compile to `wasm32-wasi` (Go, Rust, C, etc.).
- Reproducible runs where the main artifact is already `.wasm`.
- Capability-scoped execution for generated or untrusted code.

For arbitrary `pnpm test`, `cargo test`, native toolchains, or full Linux, use `local-container`, `e2b`, `hetzner`, etc.

## When to use

Use when you want a lightweight, zero-OS, capability-scoped sandbox that participates in the normal `crabbox run --id` / warmup / capsule / timing surface.

Do **not** use for legacy suites that require apt, arbitrary syscalls, heavy FS, or interactive shells.

## For agents and automation

Use `--provider wasi` for capability-scoped runs of generated/untrusted code or portable `.wasm` test binaries: `crabbox run --provider wasi -- ./my-app.wasm`. Guest sees *only* the synced checkout (preopened `/work`); no host ambient authority or POSIX surface. Provides the same "warm, sync diff, run, proof" experience as other providers but as a lightweight local zero-OS sandbox.

## Commands

```sh
# Run a pre-built wasm (after cargo build --target wasm32-wasi or equivalent)
crabbox run --provider wasi -- ./target/wasm32-wasi/debug/myapp.wasm --arg1 value

# With warmup + reuse (guest dir is kept)
crabbox warmup --provider wasi
crabbox run --provider wasi --id my-wasi -- ./my-tests.wasm

crabbox status --provider wasi --id my-wasi
crabbox stop --provider wasi my-wasi
```

## Config

```yaml
provider: wasi
target: linux
wasi:
  workdir: crabbox
  runtime: wazero          # wazero (embedded, zero-dep) or wasmtime
  guestBaseDir: ""         # optional base dir for isolated guest roots
```

Flags: `--wasi-workdir`, `--wasi-runtime`.

`guestBaseDir` defaults to the system temp directory. When set, it must be an
absolute path, and guest roots must not overlap the synced repository tree;
both are rejected before any host-side write so kept guest files can never
feed back into later sync manifests.

The guest tree is at `<guestBase or /tmp>/crabbox-wasi-<lease>/<workdir>/`. Your module sees it via preopen (typically `/work`).

## Capabilities & limitations

- Sync: archive-style manifest copy (respects excludes, Delete, preflight guards). Fingerprint skip and rsync delta not applicable (local guest copy).
- Reusable sessions and delegated run proof output are supported.
- No SSH, desktop, VNC, code-server, Actions hydration, tailscale, broker.
- `--class`/`--type` rejected.
- POSIX surface is limited (no apt, limited threads/sockets/FS in preview1; preview2/components better but not universal).
- CPU-heavy code may be 5-20% slower than native.
- wasmtime is used when `--wasi-runtime wasmtime` is set and the CLI is in PATH. Plain `.wasm` modules fall back to wazero if wasmtime is missing; `.cwasm` modules require wasmtime.

## Reproducible artifacts

A `.wasm` module plus the synced input tree is a compact replay artifact. Today this provider exposes that through normal `crabbox run`, `--sync-only`, reusable sessions, and delegated proof output. Capsules remain Actions-first failure manifests; use them when you want to replay a command through Crabbox, not as a WASI-specific package format.

## Live smoke (local, no secrets)

```sh
go build -trimpath -o bin/crabbox ./cmd/crabbox
bin/crabbox run --provider wasi -- echo 'crabbox-wasi-smoke'
bin/crabbox doctor --provider wasi --json
bin/crabbox run --provider wasi -- false || echo "expected non-zero"
```

For a real module example, see the test or compile a tiny Go program with `GOOS=wasip1 GOARCH=wasm go build`.

## Related

- Provider authoring & backends docs
- Features: capsules, sync, delegated providers
- [wasi.dev](https://wasi.dev/) (official intro + runtimes) + [WASI spec](https://github.com/WebAssembly/WASI)
- wazero / wasmtime docs

**This provider is experimental.** Usage, surface, and runtime support may change. It is a great fit for Wasm-native or agent workloads but will disappoint on traditional full-Linux test suites.
