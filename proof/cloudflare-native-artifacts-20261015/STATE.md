# Cloudflare native artifact downloads

- Task: https://github.com/openclaw/crabbox/issues/2859
- Owner: assigned Crabbox implementation worker; parent coordinator closes.
- Repo: openclaw/crabbox
- Worktree: /Users/bobbybones/Developer/worktrees/crabbox-codex-cloudflare-artifacts-20261015
- Branch: codex/cloudflare-native-artifacts-20261015
- Fresh base: 05eded31b4d8b9a96f8e56813de142f4a9255b8e
- Mode: mutate source/tests/docs only; no deployment, provisioning, or credentials.
- Status: draft preparation; full Go race gate running
- Acceptance: existing artifact flags retrieve selected binary files, enforce required outputs, retain primary failure/cleanup behavior, refuse incompatible runners, focused protocol/path tests pass.
- Stop: committed/pushed draft PR and exact-head review preparation; merge and deployment remain human gates.
- Watchdog: bounded local tests and collection timeouts; no live cloud calls.
