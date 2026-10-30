# Headless Playwright on Cloudflare Containers

Run a small synthetic browser test in a Linux container without a desktop,
external website, or application credentials. The fixture under
[`examples/cloudflare-playwright`](../../examples/cloudflare-playwright) pins
Playwright 1.61.1 with an npm lockfile. It opens two fresh browser contexts,
checks empty cookies and local storage, saves different fixture data in each,
asserts the resulting DOM, and writes two screenshots plus a JSON result.
All browser requests are fulfilled locally or aborted.

## Status and prerequisites

**The Cloudflare download commands below depend on
[the native artifact download change](https://github.com/openclaw/crabbox/pull/2860).
They are not supported by released Crabbox 0.78.0 or the main branch at this
recipe's introduction.** Both the CLI and deployed Worker must contain that
change; the Worker must advertise `CBX-CF-ARTIFACT-1`. Updating the CLI alone
does not update the deployed runner. Until the dependency is available, use
the local fixture section; do not copy the download flags into an older CLI.

Use an already configured, compatible Cloudflare Containers runner and a
source-qualified CLI. The runner credentials stay with Crabbox; the fixture
needs none and does not use `--env`. The example installs browser dependencies
inside its disposable Linux container, not into the default runner image.
See the [provider guide](../providers/cloudflare.md) for runner setup, costs,
instance sizing, login-shell environment behavior, and lease lifetime rules.

The existing [deployed artifact experiment](https://github.com/openclaw/crabbox/pull/2860#issuecomment-6244850537)
used Linux/x64, Playwright 1.61.1 and Chromium 149.0.7827.55. It proved screenshot
transport on a fresh command and an intentionally failing reused command.
It did **not** run this new two-context assertion fixture, and its cleanup
needed independent operator reconciliation. The
[immutable artifacts](https://github.com/saari-co/public-oss-proof-assets/releases/tag/crabbox-pr2860-12f3871a-cloudflare-artifacts-20261018)
are evidence for that experiment, not a new Cloudflare run of this recipe.

## Try the fixture locally

The recorded local check used Node 24.21.0 (the repository's `.node-version`
selects major 24) and npm. From the
repository root, copy only the example into a new directory. This also makes
the later Cloudflare upload independent of your repository's other files.

```sh
recipe_dir=$(mktemp -d)
cp examples/cloudflare-playwright/{package.json,package-lock.json,prepare.mjs,browser.mjs} "$recipe_dir/"
cd "$recipe_dir"
git init -q
git add package.json package-lock.json prepare.mjs browser.mjs
printf 'node_modules/\nartifacts/\n.crabbox/\n' > .gitignore
git add .gitignore

node prepare.mjs
node browser.mjs artifacts/local
```

`prepare.mjs` runs `npm ci --ignore-scripts` and the locked Playwright browser
installer, recording `npm_ci` and `browser_install` wall times in
`artifacts/preparation.json`. On Linux, use `node prepare.mjs --with-deps`
when system libraries also need installing; that explicit option runs
Playwright's system package installer and requires suitable privileges.

`artifacts/local/result.json` records the actual OS, architecture, Node,
Playwright and Chromium versions, browser wall time, and each screenshot's
size and SHA-256. The two cases must both pass for the command to return zero
and write a successful result. Browser time includes launch, assertions,
screenshots and cleanup; it is not a CPU-only measurement. Output directories
must be new, so reruns cannot silently reuse old results. Inspect both PNGs.

## Run and reuse one Cloudflare lease

Run these commands from the isolated directory above **only after the native
artifact prerequisite is satisfied**. Set `CRABBOX` to the absolute path of
the compatible CLI. Store local proof outside the upload directory:

```sh
: "${CRABBOX:?Set CRABBOX to the compatible CLI's absolute path}"
proof_dir=$(mktemp -d)

"$CRABBOX" run --provider cloudflare --type standard-2 --ttl 45m \
  --keep --lease-output "$proof_dir/lease.json" --timing-json \
  --artifact-glob 'artifacts/**' \
  --download "artifacts/preparation.json=$proof_dir/preparation.json" \
  --download "artifacts/fresh/result.json=$proof_dir/fresh.json" \
  --download "artifacts/fresh/run-1.png=$proof_dir/fresh-1.png" \
  --download "artifacts/fresh/run-2.png=$proof_dir/fresh-2.png" \
  --require-artifact artifacts/preparation.json \
  --require-artifact artifacts/fresh/result.json \
  --require-artifact artifacts/fresh/run-1.png \
  --require-artifact artifacts/fresh/run-2.png \
  --shell -- 'node prepare.mjs --with-deps && node browser.mjs artifacts/fresh'

lease_id=$(node -e 'const fs=require("node:fs"); const h=JSON.parse(fs.readFileSync(process.argv[1])); if(h.provider!=="cloudflare" || !/^cbx_[A-Za-z0-9]+$/.test(h.leaseId)) process.exit(1); process.stdout.write(h.leaseId)' "$proof_dir/lease.json")
test -n "$lease_id"

"$CRABBOX" run --provider cloudflare --id "${lease_id:?Recover the exact lease ID first}" --no-sync --keep \
  --ttl 45m --timing-json --artifact-glob 'artifacts/reused/**' \
  --download "artifacts/reused/result.json=$proof_dir/reused.json" \
  --download "artifacts/reused/run-1.png=$proof_dir/reused-1.png" \
  --download "artifacts/reused/run-2.png=$proof_dir/reused-2.png" \
  --require-artifact artifacts/reused/result.json \
  --require-artifact artifacts/reused/run-1.png \
  --require-artifact artifacts/reused/run-2.png \
  --shell -- 'node browser.mjs artifacts/reused'
```

The reused command preserves installed dependencies but launches a new browser
and two new contexts. It skips preparation deliberately; compare its recorded
browser time with `fresh.json`, not the fresh command's install-inclusive
duration. Native `--timing-json` also reports the Crabbox lifecycle. No warm
speedup is guaranteed. Retained leases continue consuming resources until
stopped or expired; reuse does not create a new lease budget.

Verify each downloaded PNG against the corresponding size/hash in the result
JSON and inspect the screenshots. Keep the native timing output and artifact
archive as evidence. A missing required artifact or failed assertion is a
failed run, even if some other files were successfully downloaded.

## Stop the exact lease

Always perform this step, including after a test or download fails. Recover
the ID from the same `lease.json` if you had to stop before the reuse command:

```sh
"$CRABBOX" stop --provider cloudflare --id "${lease_id:?Recover the exact lease ID first}"
"$CRABBOX" status --provider cloudflare --id "${lease_id:?Recover the exact lease ID first}"
```

Confirm the exact lease is stopped and independently check provider state
when required by your environment. An exited stop command is not proof that
every provider resource has disappeared; a retained stopped row is not a live
container. If creation fails without a usable handle, reconcile that attempt
before creating another lease. Do not run account-wide cleanup or remove the
shared runner to clean up this example.

Named custom images, warmup, and checkpoint/fork are already available for
later optimization. They are optional and are not prerequisites for this
small fixture.
