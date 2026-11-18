# Notes — github-core-clone-01 v4

## Kit

- Fresh kit level 20 at
  `~/workspace/tools/makefun/github-core-clone-01-kit-v4/github-core-clone-01-kit/`
  (`RULES.txt`, `competition.yaml`, `sdk/conformance/*`, `starter/*`).
- Competition: `github-core-clone-01`. Entries close 2026-11-21T06:30:00Z.
- Viewer login (from the kit's `buildScenario()`): `adaiversen`.
- Score = feature 600 + agent 200 + perf 200. House reference T_ref:
  push 1640.82 ms, clone 266.941 ms, read 2759.715 ms, write 348.965 ms.
- v3 baseline (grade-local, process driver): push 289 ms / clone 95 ms /
  read 729 ms / write 84 ms, geomean 203k µs, 193 local perf pts, 132 live
  pts → score 932. Paid cutoff #20 = 956.

## Verification status

- Go unit tests (`forge-src/forge_test.go`): hash vectors, zlib round-trip,
  delta apply, pack round-trip (plain + REF_DELTA), JSON parser,
  contents/tree round-trip, fetch planning (full/incremental/nothing-new/
  depth-1/deepen-2), pkt-line framing. All passing (`go test ./...`).
- Smoke test (`tests/smoke.mjs`, fake env not needed — boots the forge
  directly): REST, real-git clone/push/shallow-clone, PR merge, GraphQL
  (viewer, repository fields, introspection, create/close issue,
  pagination, batch, invalid field), MCP (discover, tools/list, tools/call
  incl. unknown-tool and no-auth and parse-error). All passing.
- `node cli/makefun.mjs check`: (to be filled after the run).
- `grade-local` (process driver, Docker is not installed on this VM):
  (to be filled after the runs — per-trial per-workload p50 + points +
  score, median of ≥3 trials, determinism check).

## Live-variance caveat

Local grade-local numbers overstate live performance: live runs ~2.55×
slower than local (gVisor + shared nodes). The brief's verified model
(live pts ≈ formula applied to local × 2.55) reproduces v3's live 132
exactly. Local targets in STRATEGY.md are set at the 50-point clamp per
workload, so the entry carries margin against that variance, but the live
score is a model prediction, not a measurement, until the server grades it.

## Build

- `go 1.27.4` (`~/go-toolchain/go`), `node v24.22.1` on this VM (kit
  says Node 22; the launcher uses only stable Node APIs).
- `./build.sh` → `forge` (static, stdlib-only, ~3.0 MiB, under the 8 MiB
  per-file cap; whole tree well under 16 MiB).
- Repo-local git identity: `Ci3hu1 <joni07683@gmail.com>`.
