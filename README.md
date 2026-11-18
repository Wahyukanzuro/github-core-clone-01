# GitForge — a GitHub-compatible git forge

[![github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Wahyukanzuro/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Wahyukanzuro/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01)

A single-process GitHub-compatible git forge, written for the
[github-core-clone-01](https://make.fun/bounty/github-core-clone-01) competition.

## What it serves

- **Git smart HTTP** — `git-upload-pack` protocol v2 (`ls-refs`, `fetch` with
  shallow/deepen/filter) and `git-receive-pack` v0, at `/{owner}/{repo}.git`,
  with a from-scratch object model (blobs, trees, commits, tags, SHA-1 ids,
  pack files, pkt-line framing) built on the Go standard library only.
- **REST v3** — repositories, branches, commits, contents, refs, labels,
  issues, comments and pull requests (including squash merge with real
  three-way merge-conflict detection) at `/api/v3`.
- **GraphQL v4** — viewer, repository, issue and pull-request queries and
  mutations, connection pagination, and schema introspection at `/api/graphql`.
- **MCP** — JSON-RPC 2.0 over Streamable HTTP at `/mcp` (revision
  `2026-07-28`, with a `2025-11-25` handshake fallback): `create_repo`,
  `get_repo`, `create_issue`, `list_issues`, `create_pull_request`,
  `merge_pull_request`, acting on the same in-memory store as REST.

## Layout

- `main.ts` — the entrypoint. Spawns the vendored `forge` binary next to it
  and forwards stdio and the exit code.
- `go/` — the Go source (one module, standard library only, no cgo). Build
  with `CGO_ENABLED=0 go build -ldflags="-s -w" -o ../forge .`
- `.makefun/owner.json` — the payout wallet for this entry.

## Notes

- All state lives in memory; every timestamp in a response derives from
  `MAKEFUN_TRIAL_NOW`, never the wall clock; iteration order is
  deterministic, so a replayed trial leaves the same state.
- No network access, no model calls, no git binary: the forge speaks the
  wire protocols itself.
