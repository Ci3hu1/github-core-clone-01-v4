# Strategy — github-core-clone-01 v4

## What this entry is

A static Go binary (stdlib only) plus a thin `main.ts` launcher, speaking
REST v3, GraphQL v4, MCP, and git smart HTTP against one in-memory store.
v3 scored 932 live (feature 600 + agent 200 + perf 132); the gap to the
paid cutoff (#20, 956) is 24 perf points, all of it in the four benchmark
workloads. Feature and agent are already maxed in v3 and must stay maxed
here — this entry changes nothing about conformance behaviour, only speed.

## Scoring, and where the points come from

Score = feature 600 + agent 200 + perf 200. Perf per workload is
`round(clamp(25 − 12.5·log2(t / T_ref), 0, 50))` over the median round time
`t`, summed over push / clone / read / write (max 50 each). Live runs
~2.55× slower than local (gVisor + shared nodes); the verified model is
live points ≈ formula applied to local × 2.55. Local targets (v4):

| workload | v3 local p50 | v3 live pts | v4 local target p50 | v4 local pts |
| --- | --- | --- | --- | --- |
| push | 289 ms | ~39 | ≤207 ms | 50 |
| clone | 95 ms | ~27 | ≤68 ms | 50 |
| read | 729 ms | ~32 | ≤520 ms | 50 |
| write | 84 ms | ~34 | ≤60 ms | 50 |
| total (geomean) | 203k µs | 132 | ≤145k µs | ≥198 local |

Target live perf ≥156 → score ≥956. Local points past the 50/workload clamp
are not chased; extra speed is margin against live variance. Local numbers
overstate live performance — see NOTES.md for the honest caveat.

## Measured edge (how the speed is won)

1. **No framework tax.** Raw TCP HTTP/1.1, hand-rolled JSON, one `Write`
   per response, `TCP_NODELAY`, keep-alive. No `net/http`, no
   `encoding/json`, no routing tables, no middleware. The read workload
   (2,000 sequential GETs on one warm issue, one keep-alive connection) is
   dominated by per-request parse/render cost; the hot path serves
   pre-serialized cached issue bytes with a map lookup and nothing else.
2. **Response cache + single-flight.** GETs (REST + git advertisement)
   and repeated identical upload-pack POSTs (the clone workload: 50
   concurrent depth-1 clones of one tip) share one build, keyed by a
   global generation counter that bumps only after genuinely mutating
   requests. Upload-pack POST is read-only and never bumps the counter.
   Concurrent identical fetches wait on one build instead of building 50
   packs.
3. **Cheap packs.** The server never sends deltas — plain objects only
   (the conformance client verifies resolved objects, so this is free).
   Packs are written with `flate.BestSpeed`. The clone workload's pack is
   3 objects (tip commit + tree + blob); the cache makes 49 of 50 clones
   a memcpy.
4. **Exact inflate.** Go's flate over-reads, so a counting byte reader
   plus an Adler-32 scan with re-inflate verification pins each zlib
   stream end exactly. This is correctness (pack boundaries), not speed —
   but it avoids the double-inflate-everything fallback a naive reader
   needs.
5. **Small binary, fast start.** `go build -trimpath -ldflags="-s -w"`,
   ~3.0 MiB. Boot time is not scored, but a small binary keeps the whole
   entry under the size caps with room to spare.

## Entry / exit rules (what the entry does and does not do)

- The entry is a forge, not a client: it never initiates requests, never
  touches the network except to answer the harness, and never persists
  anything. State is in-memory and dies with the process.
- It answers exactly the surfaces the suite exercises: REST v3, GraphQL
  v4, MCP (2026-07-28 + legacy initialize), git smart HTTP v2/v0.
  Anything else is a JSON 404.
- Determinism is a rule, not a preference: fixed timestamps, creation-order
  ids, sorted listings, no wall clock or unseeded randomness in any
  response. Replay must match byte for byte.
- The fast paths are all read-only caches invalidated by the generation
  counter; a render never sees a half-applied mutation (one RWMutex,
  writes under the write lock).

## Tried and dropped

- **Deltas on the wire out.** Sending OFS/REF deltas would shrink packs,
  but the packs the workloads serve are already tiny (3 objects for the
  clone workload; the read/write/push workloads carry no packs at all),
  and delta search costs more CPU than the bytes save. Plain objects only.
- **A global response cache keyed by URL.** Dangerous: issue bodies change
  on PATCH/comment, and a stale cache would fail conformance. The cache is
  per-object (issue bytes rebuilt on mutation) plus the generation-keyed
  pack cache — narrow, provably fresh.
- **Lock-free/sharded read path.** A single global RWMutex was suspected
  of slowing the read workload; profiling showed the read workload is
  sequential on one connection, so lock contention is nil. The mutex stays
  (correctness first); no sharding was needed.
- **Chasing boot time or the harness client.** Boot is unscored and the
  harness client is fixed. No effort spent there.
- **Cosmetic README/docs changes.** No score impact; skipped.

## Expected outcomes

- Feature 600 and agent 200 in every trial (all 120 conformance checks +
  15 MCP checks pass; the smoke test in `tests/smoke.mjs` covers the same
  surfaces with a real git client).
- Local perf ≥198/200 (per-workload p50 at or under the targets above),
  which the ×2.55 live model maps to ≥156 live → score ≥956.
- Determinism: two grade-local runs produce byte-identical results.
- If a verification step fails and cannot be fixed by rewriting this
  entry's own code, that is reported as a failure, not papered over.

## Risks

- Live variance: the ×2.55 factor is a model from v3, not a guarantee.
  The margin is the local clamp headroom (local 198 vs the 156 needed
  live) plus the fact that local targets were set at the 50-point clamp,
  not at the live break-even.
- The kit client and real git disagree on two protocol corners (v2 fetch
  `acknowledgments` only when the client sent haves; push report lines
  each in their own pkt-line inside band 1). Both are handled to satisfy
  real git, which is a superset of what the kit client accepts.
