# Running the MPP E2E manually

This is the recipe for closing the loop on the MPP 402 → pay → provision
flow against a funded Tempo wallet. The worker side has been validated to
emit a correct challenge in the right wire format. Settlement requires a
funded account on the chain you point the broker at; this guide stays
local + uses your own keychain so the private key never leaves your box.

## 1. Configure local secrets

In `worker/.dev.vars` (gitignored):

```text
HETZNER_TOKEN=hcloud_...
CRABBOX_SHARED_TOKEN=dev-admin-token-do-not-deploy
CRABBOX_SESSION_SECRET=dev-session-secret-do-not-deploy
CRABBOX_DEFAULT_ORG=crabbox-trial
CRABBOX_MPP_RECIPIENT=0x...   # your wallet
CRABBOX_MPP_DECIMALS=6
CRABBOX_MPP_SECRET_KEY=dev-mpp-secret-do-not-deploy
CRABBOX_MPP_REALM=localhost:8787   # see Realm pinning in payments-mpp.md
# CRABBOX_MPP_TESTNET=true    # uncomment if running on Tempo testnet
```

## 2. Start the broker

```sh
cd worker
npx wrangler dev --port 8787 --local
```

Verify it boots and lists `env.CRABBOX_MPP_*` bindings.

## 3. Promote a Hetzner snapshot (so the lease boots warm)

You'll want a real snapshot ID. Use the one from the smoke test or bake a
fresh one. Then promote it through the admin route:

```sh
curl -s -X POST http://localhost:8787/v1/images/<snapshot_id>/promote \
  -H "Authorization: Bearer dev-admin-token-do-not-deploy" \
  -H "Content-Type: application/json" \
  -d '{}'
```

The DO writes `image:hetzner:promoted` and subsequent unauthenticated
lease requests will boot from that snapshot.

## 4. Probe the 402 challenge (no wallet yet)

```sh
curl -i -s -X POST http://localhost:8787/v1/leases \
  -H "Content-Type: application/json" \
  -d '{"provider":"hetzner","class":"standard","serverType":"cpx22","ttlSeconds":1200,"sshPublicKey":"ssh-ed25519 AAAA...example"}'
```

You should see:

```
HTTP/1.1 402 Payment Required
WWW-Authenticate: Payment id="...", realm="localhost:8787", method="tempo",
                  intent="charge", request="<base64URL>", expires="..."
```

Decode the base64URL after `request=` to see the exact `{amount, currency,
recipient, methodDetails: {chainId}}` the broker is asking for. Confirm
recipient matches your wallet, currency matches pathUSD on your chain,
and chainId is right (4217 = Tempo mainnet).

## 5. Sign + retry with mppx

Install once:

```sh
npm i -g mppx
```

Make sure your account is configured (the keychain entry, not the public
address). For an existing funded wallet, import via:

```sh
mppx account create -a my-wallet     # generate new
# OR
mppx account import -a my-wallet     # paste private key — never share it
```

Then run the same request through mppx — it handles 402 → sign → retry
automatically:

```sh
mppx -a my-wallet -X POST \
  -J '{"provider":"hetzner","class":"standard","serverType":"cpx22","ttlSeconds":1200,"sshPublicKey":"ssh-ed25519 AAAA..."}' \
  http://localhost:8787/v1/leases
```

On success the broker:
1. Verifies the credential against your funded wallet on Tempo.
2. Calls Hetzner with the promoted snapshot ID.
3. Returns `201 Created` with the lease record (host, sshUser, sshPort,
   workRoot, ttl, etc.) and an MPP receipt header.

## 6. SSH in and verify the bake

```sh
ssh -i <your-test-key> -p <sshPort> <sshUser>@<host> 'rustc --version; cat /etc/crabbox-marker'
```

If the snapshot was the trial one, you'll see `rustc 1.95.0` and the
marker file.

## 7. Tear down

The lease has a TTL. It auto-expires + auto-deletes the Hetzner server
on the alarm. To force release manually you'd need the bearer admin
token and:

```sh
curl -s -X POST http://localhost:8787/v1/leases/<lease_id>/release \
  -H "Authorization: Bearer dev-admin-token-do-not-deploy" \
  -d '{"delete":true}'
```

## What this proves

1. mppx server-side bundles cleanly into a Cloudflare Worker.
2. The 402 challenge / credential / receipt round trip works.
3. A wallet-only agent can lease a Hetzner runner with no admin auth.
4. Snapshot warm-boot delivers measurable speedup (smoke test: ~15s
   extra boot vs. ~31s+ saved bake time per cold lease).

## What's deferred

- `crabbox` Go CLI handling 402 directly (today the path is curl/mppx
  shell-out; Go integration is a follow-up).
- Per-class promoted-image tag namespace (today is one slot per
  provider).
- Lease-bound bearer token returned alongside the receipt so heartbeat
  / release / runs work for an MPP-only client.
