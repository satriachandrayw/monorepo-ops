# monorepo-ops

Public operations monorepo for reusable CI plugins and infrastructure tooling.

## Components

- [`plugins/woodpecker-git-resilient`](plugins/woodpecker-git-resilient/) — a
  timeout- and retry-aware Woodpecker Git clone plugin for intermittent Git pack
  transfer stalls.

Container images are published to GHCR under the `satriachandrayw` namespace.
Pin image digests when consuming them in trusted clone steps.
