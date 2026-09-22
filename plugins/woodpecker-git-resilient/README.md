# Resilient Git clone plugin

A small, trusted Woodpecker clone plugin maintained in the public
[`monorepo-ops`](https://github.com/satriachandrayw/monorepo-ops) repository.
keeps the clone contract intentionally narrow and uses the system `git`
executable rather than implementing Git itself.

## MVP behavior

- Initializes a shallow all-blob repository and fetches the exact `CI_COMMIT_SHA`.
- Supports the repository SSH clone URL with pinned private key and host key.
- Writes HTTPS netrc credentials when the standard clone URL is used.
- Adds a hard fetch timeout and an inactivity timeout.
- Kills the complete `git`/`ssh` process group when a timeout fires.
- Retries from a clean `.git` directory with exponential backoff.
- Emits a bounded diagnostic for every attempt, including whether the failure was
  a Git error, idle timeout, or total command timeout.
- Uses bounded SSH connection and keepalive settings so a dead SSH session fails
  early enough for the next clean retry.
- Supports an optional persistent local mirror. After the mirror is warmed once,
  repeated clones of the same commit copy from local Git objects instead of
  opening a GitHub transport. The mirror is protected by a cross-process lock
  so multiple Woodpecker agents can share one named Docker volume.
- Supports GitHub SSH over port 443 through `ssh.github.com` when the
  `PLUGIN_SSH_HOST_KEY_ALIAS` setting is set to `github.com`; this avoids networks
  that interfere with long-lived SSH sessions on port 22.
- Defaults to Git protocol v0; set `PLUGIN_PROTOCOL_VERSION=2` to compare.
- Does not print credential values or credential-bearing URLs.

This is an MVP, not a replacement for `woodpeckerci/plugin-git`. Submodules,
LFS, tag events, and pull-request merge behavior remain deliberately out of
scope until the timeout/retry seam is proven against the current failure.

## Trusted clone requirement

The image handles clone credentials and must be added to Woodpecker's trusted
clone-plugin allowlist before it is used by a workflow. Pin the published image
by digest when promoting it beyond a canary.

## Settings

| Variable | Default | Meaning |
| --- | --- | --- |
| `PLUGIN_USE_SSH` | `false` | Use `PLUGIN_REMOTE_SSH` and SSH credentials |
| `PLUGIN_REMOTE` | `CI_REPO_CLONE_URL` | HTTPS remote |
| `PLUGIN_REMOTE_SSH` | `CI_REPO_CLONE_SSH_URL` | SSH remote |
| `PLUGIN_DEPTH` | `1` | Shallow fetch depth; `0` means full fetch |
| `PLUGIN_ATTEMPTS` | `3` | Fetch attempts |
| `PLUGIN_BACKOFF` | `5s` | Initial retry delay, doubled per retry |
| `PLUGIN_FETCH_TIMEOUT` | `10m` | Maximum time for one Git command |
| `PLUGIN_IDLE_TIMEOUT` | `90s` | Maximum time without Git output |
| `PLUGIN_PROTOCOL_VERSION` | `0` | Git protocol version |
| `PLUGIN_SSH_CONNECT_TIMEOUT` | `10s` | SSH connection timeout |
| `PLUGIN_SSH_SERVER_ALIVE_INTERVAL` | `5s` | SSH keepalive interval |
| `PLUGIN_SSH_SERVER_ALIVE_COUNT_MAX` | `3` | Keepalive failures before SSH closes |
| `PLUGIN_SSH_HOST_KEY_ALIAS` | empty | SSH `HostKeyAlias` used to match a pinned known-hosts entry when the transport hostname differs |
| `PLUGIN_MIRROR_PATH` | empty | Absolute path to a persistent bare Git mirror; empty disables the mirror |
| `PLUGIN_MIRROR_DEPTH` | `5` | Shallow depth used when updating the persistent mirror |
| `PLUGIN_SSH_KEY_PRIVATE` | empty | Private key content for SSH clone |
| `PLUGIN_SSH_HOST_KEY` | empty | Pinned known-hosts content |

The plugin also consumes Woodpecker's normal `CI_WORKSPACE`, `CI_COMMIT_SHA`,
`CI_COMMIT_BRANCH`, and `CI_NETRC_*` metadata variables.
