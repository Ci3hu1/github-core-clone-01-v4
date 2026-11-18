# github-core-clone-01 v4 — a GitHub-compatible git forge

A static, stdlib-only Go binary (`forge`) plus a thin Node launcher
(`main.ts`) that speaks GitHub's REST v3, GraphQL v4, MCP (JSON-RPC 2.0,
revision 2026-07-28), and git smart HTTP (upload-pack v2/v0,
receive-pack v0) against one in-memory store — built for the make.fun
bounty [github-core-clone-01](https://make.fun/bounty/github-core-clone-01).

## Layout

- `main.ts` — entrypoint. Spawns `./forge` (fallback ladder: direct exec →
  copy to tmp → memfd via python3), reads its `PORT <n>` line, registers
  the port with `$MAKEFUN_ENV_URL/register`, polls `/done`, stops the
  forge, exits 0. Plain Node, no third-party packages.
- `forge` — the static Go binary (stdlib only, ~3.0 MiB).
- `forge-src/` — the Go source, hand-rolled everything: raw TCP HTTP/1.1
  server, hand-rolled JSON parser/writer, pkt-line + pack read/write,
  GraphQL lexer/parser/executor, MCP JSON-RPC. No `net/http`, no
  `encoding/json`, no third-party imports.
- `build.sh` — reproducible build: `go build -trimpath -ldflags="-s -w"`.
- `tests/smoke.mjs` — boots the forge and exercises REST + git (real git
  CLI) + GraphQL + MCP. Run with `node tests/smoke.mjs`.
- `STRATEGY.md` — the measured edge and what was tried and dropped.
- `NOTES.md` — live numbers, checks, kit version.

## Build and run

```sh
./build.sh            # produces ./forge
./forge               # prints PORT <n>, serves until killed
node tests/smoke.mjs  # full smoke test
```

With the harness: `MAKEFUN_ENV_URL=http://... node main.ts` (Node 22).

## Design notes

- **Store**: in-memory, one global RWMutex; a render never sees a
  half-applied mutation. Deterministic: fixed timestamps
  (`2025-01-01T00:00:00Z`), creation-order ids, no wall clock or unseeded
  randomness in any response.
- **Perf**: response cache + single-flight for GETs and repeated identical
  upload-pack fetches, keyed by a generation counter that bumps only on
  genuinely mutating requests. Hot issue responses are pre-serialized and
  served as cached bytes. One `Write` per response, `TCP_NODELAY`,
  keep-alive, `Expect: 100-continue` and chunked bodies supported.
- **Git**: pkt-line framing, pack parse/write (plain objects on the wire;
  OFS/REF deltas resolved on ingest), upload-pack v2 sections
  (`acknowledgments` only when the client sent haves, `shallow-info`,
  `packfile`) and v0, receive-pack v0 with a pkt-lined report-status
  stream inside sideband band 1. Inflate pins the zlib stream end exactly
  (Adler-32 scan + re-inflate verification) because Go's flate over-reads.
  Packs are written with `flate.BestSpeed`.
- **GraphQL**: small lexer/parser/executor for exactly the documents the
  clients send, including `__type` introspection, `node(id:)` with inline
  fragments, batch operations, and cursor pagination.
- **MCP**: `server/discover` (revision 2026-07-28) with the legacy
  `initialize` fallback; six tools (`create_repo`, `get_repo`,
  `create_issue`, `list_issues`, `create_pull_request`,
  `merge_pull_request`) acting on the same store as REST; `tools/list` is
  byte-identical across calls.

## License

MIT — see `LICENSE`.
