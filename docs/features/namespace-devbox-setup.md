# Namespace Devbox Setup

Read when:

- installing the Namespace `devbox` CLI for Crabbox;
- storing a Namespace auth token for automation;
- live-checking that Namespace auth, Devbox SSH, and Crabbox agree.

Crabbox shells out to the Namespace `devbox` CLI. The CLI owns browser login,
Devbox creation, SSH config, shutdown, and deletion. Crabbox reads the generated
SSH config and then uses normal SSH and rsync.

## Install

Install the upstream Devbox CLI:

```sh
curl -fsSL https://get.namespace.so/devbox/install.sh | bash
devbox login
devbox auth check-login
```

For non-interactive shells, open the browser token page, copy the token, and
store it in the shell profile without printing it:

```sh
open https://cloud.namespace.so/login/token

cat >> ~/.profile <<'EOF'
# Namespace auth
export NAMESPACE_TOKEN='<token>'
export NSCLOUD_TOKEN="$NAMESPACE_TOKEN"
EOF
```

Sanity-check the profile without exposing the token:

```sh
zsh -lc 'source ~/.profile; test -n "$NAMESPACE_TOKEN"; test "$NSCLOUD_TOKEN" = "$NAMESPACE_TOKEN"'
```

## Live Check

Verify Namespace account access:

```sh
devbox auth check
devbox image list -o json
devbox list -o json
```

Verify a Devbox can expose SSH:

```sh
devbox create --name crabbox-smoke --image builtin:base --size s --auto_stop_idle_timeout 10m --activate
ssh crabbox-smoke.devbox.namespace 'command -v git && command -v rsync && command -v tar'
```

Then verify Crabbox against the same Devbox:

```sh
crabbox status --provider namespace-devbox --id crabbox-smoke
crabbox run --provider namespace-devbox --id crabbox-smoke --shell 'echo crabbox-live-ok'
crabbox stop --provider namespace-devbox crabbox-smoke
```

## Notes

- Default image: `builtin:base`.
- The generated SSH host is `<name>.devbox.namespace`.
- Namespace writes SSH snippets under `~/.namespace/ssh/` and includes them
  from `~/.ssh/config`.
- `crabbox stop --provider namespace-devbox` and
  `crabbox cleanup --provider namespace-devbox` remove Crabbox-owned
  `crabbox-*.devbox.namespace.{ssh,key}` files.
- `devbox list -o json` prints non-JSON text when no Devboxes exist; Crabbox
  treats that as an empty list.

Related docs:

- [Namespace Devbox](namespace-devbox.md)
- [Provider: Namespace Devbox](../providers/namespace-devbox.md)
