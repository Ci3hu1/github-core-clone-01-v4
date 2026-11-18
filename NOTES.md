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
  depth-1/deepen-2), pkt-line framing. All passing (`go test ./...`,
  `go vet` clean).
- Smoke test (`tests/smoke.mjs`, boots the forge directly): REST, real-git
  clone/push/shallow-clone, PR merge, GraphQL (viewer, repository fields,
  introspection, create/close issue, pagination, batch, invalid field),
  MCP (discover, tools/list, tools/call incl. unknown-tool and no-auth and
  parse-error). All passing (`SMOKE OK`).
- Kit conformance runner (`sdk/run.ts`, fresh forge, no `--perf`):
  **feature 600, agent 200, all 120 + 15 checks pass.**
- `grade-local` (process driver; Docker is not installed on this VM),
  commit `b2d16376f15588b0b1b318703698479d67d0765a`, 3 trials:
  - Trial 1: feature 600, agent 200, perf 197, geomean 177,974 µs, score 997
  - Trial 2: feature 600, agent 200, perf 190, geomean 208,272 µs, score 990
  - Trial 3: feature 600, agent 200, perf 189, geomean 218,517 µs, score 989
  - Median: feature 600, agent 200, perf 190, score 990.
- Direct `sdk/run.ts --perf` runs (fresh forge, per-workload detail):
  - Run A: push 320.5 ms (50), clone 61.3 (50), read 569.5 (50),
    write 101.4 (47), geomean 183,520 µs, perf 197, score 997.
  - Run B: push 306.6 (50), clone 92.2 (44), read 756.6 (48),
    write 93.6 (49), geomean 211,496 µs, perf 191, score 991.
  - Per-workload medians (A+B): push ~313 ms, clone ~77 ms,
    read ~663 ms, write ~97 ms.
- `node cli/makefun.mjs check`: **6 passed, 1 failed, 14 skipped,
  5 checked at submission.** The one failure is `transcripts_present`
  (no transcript bundle was supplied — bundling/submission is the
  parent's job). The 5 at-submission items (repo_public, repo_owner,
  repo_created_after_open, commit_in_repo, duplicate_snapshot) cannot be
  verified offline; the repo is public, created 19 Nov 2026 (after the
  14 Nov opening), and commit b2d1637 is pushed to master. Note:
  repo_owner passes only if submission comes from the GitHub account
  owning the repo (Ci3hu1) or a `.makefun/owner.json` with a linked
  wallet is committed.
- Determinism: conformance checks are byte-deterministic (same 600/200
  in every run); perf times vary ±20% run to run (shared-VM noise),
  as expected for timing, not for results.

## Live projection (model, not a measurement)

Applying the brief's verified model (live pts = formula on local × 2.55)
to the per-workload medians above: push 38 + clone 31 + read 34 +
write 31 = **~134 live perf → score ~934**. The local targets were NOT
all met: clone beat its target in run A (61 ms) but not run B (92 ms);
push (~313 ms vs ≤207 target), read (~663 vs ≤520), write (~97 vs ≤60)
did not. Local median perf 190 vs the 198 target.

Why: profiling (pprof + raw-socket probes) shows the server is at its
floor — ~23–25 µs per REST request, ~282 µs per push (advertisement +
receive-pack) — and the bench numbers are ~85–90% harness-client time
(undici fetch + per-request AbortController/Headers/URL/JSON work, plus
a kit-client push = advertisement GET + POST). Against a canned server
doing zero git work, the kit client still takes ~3.4 ms/push. No
server-side change can move the client-dominated workloads materially:
halving the server's push cost would gain ~1 live point. The two bugs
found by conformance (annotated-tag object missing from clone packs;
merge-conflict using the PR's creation-time baseSha instead of the live
base tip) were fixed and verified — feature/agent are maxed, which is
where the entry's real, controllable points are.

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
