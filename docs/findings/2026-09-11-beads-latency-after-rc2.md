# Beads latency after bd 1.3.0-rc.2 — measurement findings (2026-09-11, 00:25-01:31)

Task: agent-forge-900q follow-up. Question: after installing bd 1.3.0-rc.2 (00:25 CEST,
`~/.local/opt/bd/1.3.0-rc.2`, symlink `~/.local/bin/bd`), did beads latency return to a
healthy level (target: reconcile p50 ~<2 s in a quiet window), is the native store actually
used, and what dominates now?

All measurements were READ-ONLY against the live system (log tail via byte offsets, timed
`bd list`/`bd show` subprocesses, /proc/pressure, ps). No services restarted, no configs
touched, no bd mutations.

## TL;DR

1. **The native store is NOT used — and installing rc.2 could not change that.**
   The `native_open` gate fails on the schema check inside gc itself: gc links
   `github.com/steveyegge/beads v1.1.1-0.20260805093327` (go.mod line 27), whose migration
   registry knows schema ≤ v59, while every rig DB is at v66. The message
   `"schema version mismatch: database is at v66, binary knows up to v59 (7 migrations ahead)"`
   refers to gc's embedded library, not the external `bd` binary. Evidence: the supervisor was
   restarted at 00:47 (PID 130698, after the rc.2 install) and *still* logged
   `native_store_unavailable` 13 times between 01:00 and 01:05, across all 7 scopes. Every gc
   beads operation goes through the BdStore fallback (subprocess `bd` CLI). Fixing this
   requires a gc-side change (go.mod bump + rebuild/redeploy) — a code change, outside this
   task's constraints.
2. **Reconcile latency is dominated by external host load.** During the whole measurement
   window load average was 18-49 (12 cores; sampled min 21.7, max 55.5 between 01:18-01:30),
   PSI cpu `some avg60` 50-67%. Post-fix p50s (9-19 s) are *worse* than the 18:00-24:00 storm
   p50s (5-7 s) simply because load was higher, not because of a regression.
3. **The `bd` subprocess itself is fast even under this load**: `bd list` 1.1-2.0 s,
   `bd show` 0.09-0.54 s, with **rc.1 ≈ rc.2** (rc.2 slightly faster in most pairs). rc.1 does
   NOT error on v66 reads — list and show work fine; the schema wall presumably only bites on
   write/migration paths (not tested: read-only task).
4. **Bead count is at the scaling wall**: rigs at 4950-5114 active beads, above the
   `beads.cache.scan_large` telemetry threshold (2500) and at the LARGE cadence boundary
   (5000). The code comment on `cacheReconcileScanWarnThreshold` documents the previous
   incident: 3272 active beads ≈ 11 MB JSON ≈ ~2 s bd latency per cycle (ga-698fl2). The PoE
   rigs are now ~50% beyond that point.
5. **The acceptance target ("reconcile p50 ~<2 s in a quiet window") could not be certified:
   no quiet window existed during measurement** (load never below ~18). The city store
   (`rig=(no-prefix)`, ~2340-2381 beads) shows the floor is healthy in calm seconds — 82-610 ms
   — but rigs never dropped below ~4.5 s even in their best cycles.

## Method

- Log: `/home/morts/.gc/supervisor.log` (>48 MB, no rotation since 2026-09-07 archive).
  Windows cut by byte offset (`grep -b -m1` for the boundary timestamp, then `tail -c +N`),
  re-parsed with a Go-duration-aware script (`291ms` / `4.83s` / `1m2.992s` → ms).
- Windows: W0 = 2026-09-10 00:00-18:00, W1 storm = 18:00-24:00 (rigs migrated to v66 through
  the evening, rc.2 released upstream 18:04), W2 = 00:00-00:25 (pre-install), W3 = 00:25+.
  W3 split again at 00:47 (supervisor restart).
- Active probes: ≤6 timed subprocesses per rig per binary, spaced ≥2 min, from the rig
  directories (`/home/morts/dev/PoE/poe_pricer`, `/home/morts/dev/PoE/poe_craft_engine`).
- Load correlation: background sampler (uptime + /proc/pressure/cpu + top-CPU processes, every
  30 s, 01:18-01:30), plus ps snapshots during slow-reconcile episodes.

## Numbers

### Reconcile `took=` per window (ms)

| window | rig | cadence | n | min | p50 | p95 | max |
|---|---|---|---:|---:|---:|---:|---:|
| W1 storm 18:00-24:00 | (no-prefix) | both | 181 | — | 4699 | 58179 | 134577 |
| W1 storm | poe2_pricer | both | 227 | — | 6544 | 18311 | 40900 |
| W1 storm | poe_craft_engine | both | 171 | — | 7088 | 16862 | 42358 |
| W2 00:00-00:25 (pre-install) | poe2_pricer | both | 13 | — | 7417 | 12013 | 14158 |
| W2 | poe_craft_engine | both | 12 | — | 7027 | 17635 | 19683 |
| W3 00:25-00:47 (post-install, old supervisor) | poe2_pricer | both | 22 | 3318 | 8882 | 11913 | 14128 |
| W3 pre-restart | poe_craft_engine | both | 21 | 5531 | 9143 | 13247 | 18768 |
| W3 00:47-01:31 (post-restart) | (no-prefix) | both | 16 | 91 | 5482 | 56242 | 95037 |
| W3 post-restart | (no-prefix) | bead-count | 12 | 82 | 1561 | 58827 | 59717 |
| W3 post-restart | poe2_pricer | both | 23 | 4915 | 10126 | 20029 | 32233 |
| W3 post-restart | poe_craft_engine | both | 18 | 4572 | 11950 | 30712 | 36076 |

Outliers worth naming: 3m8.7s at 00:45:29 and 1m35.0s at 01:16:50, both `(no-prefix)`;
both landed on load spikes (44-49). Calm-second counterexamples in the same window:
82 ms, 91 ms, 94 ms, 251 ms, 609 ms — all `(no-prefix)`. The rigs' floor (~4.5-5.8 s) sits an
order of magnitude above the city store's floor.

### Dashboard API ([memory] source, seconds)

| window | endpoint | n | p50 | p95 | max |
|---|---|---:|---:|---:|---:|
| W1 storm | beads | 2560 | 6.00 | 13.81 | 49.81 |
| W1 storm | agents | 2203 | 6.36 | 42.09 | 60.00 |
| W3 post | beads | 223 | 7.43 | 22.33 | 43.64 |
| W3 post | agents | 185 | 13.02 | 55.87 | 59.49 |

Memory-sourced endpoints serving in 6-13 s p50 confirm the supervisor process itself is
CPU-starved by external load — this is not a beads-backend property.

### Active subprocess probes (load 18-34)

| probe | rc.1 | rc.2 |
|---|---:|---:|
| pricer `bd list` (4955 rows) | 1.971 s, 1.791 s | 1.764 s, 1.471 s |
| craft `bd list` (5018 rows) | 1.123 s | 1.758 s |
| pricer `bd show <bead>` | 0.538 s | ~0.09 s |
| craft `bd show <bead>` | 0.281 s | 0.199 s, 0.227 s |

rc.1 on v66 DBs: no error, normal output on both `list` and `show`.

### Degradation counters

| signal | W1 storm (18:00-24:00) | W3 post (00:25-01:31) |
|---|---:|---:|
| `slow_storage_degraded` | 148 | 10 |
| `native_store_unavailable` | many (continuous) | 14 logged; 13 of them AFTER the 00:47 restart |

The 15x drop in `slow_storage_degraded` is the clearest positive effect of the rc.2 install.

### Load during the window

- 01:13 — load 26.6/34.1/38.4; PSI some avg60=49.8; top: poepricer deal_scan 67%,
  shadow_score 60%, health 50%, `gc supervisor run` 52%, dolt sql-server (craft) 30%,
  dolt sql-server (pricer) 24%.
- 01:18-01:30 sampler — load 1-min 21.7-55.5, PSI some avg10 up to 70.8%; repeated top
  offenders: python (poepricer suite, 44-99%), gc 58%, bd 50-88% (subprocess storms),
  node 63-68%, chrome-headless 127% (single spike).

The dolt sql-server per rig is a constant background consumer (23-30% CPU each over 15.5 h
uptime); the city `gc-agentforge` dolt server sits at ~0% CPU — consistent with the city
store's 82-609 ms reconciles vs the rigs' ≥4.5 s floor.

## What dominates, ranked

1. **External CPU pressure** (poepricer pricing sweeps, fleet agents, chrome) — load 18-49 on
   12 cores, PSI avg10 up to 70%. Explains the bimodal reconcile times and the 56-60 s
   episodes; explains why post-fix p50 is worse than storm p50.
2. **Native store unavailable by construction** (gc's embedded beads library at v59 vs DBs at
   v66) — every gc beads op pays subprocess spawn + CLI startup instead of an in-process
   store.
3. **Rig store size** — 4950-5114 active beads per rig vs the documented ~2 s-at-3272-beads
   scaling wall; both rigs are above the scan_large telemetry threshold.
4. **dolt sql-server cost** — query latency + its own CPU appetite, starved alongside
   everything else.

## Verdict and recommendation for 900q

- The rc.2 install removed the *error-level* breakage (slow_storage_degraded 148 → 10) but
  **did not restore the native store and did not bring reconcile p50 back to ~2 s** — the
  latter could not even be evaluated, because no quiet window existed (load ≥18 the whole
  time). Closing 900q as "latency healthy" would not be supported by this data.
- Recommended before closure:
  1. **gc side (owner decision, code change):** bump `github.com/steveyegge/beads` in go.mod
     to ≥ 1.3.0-rc.2 and rebuild/redeploy gc so the native gate passes again. Until then
     `native_store_unavailable` per scope is expected noise, not a fault signal.
  2. **Rig hygiene:** archive/trim active beads in poe2_pricer and poe_craft_engine (both
     >2500 threshold; pricer crossing the 5000 LARGE cadence boundary).
  3. **Re-measure in a genuinely quiet window** (load < ~5) to certify the p50 <2 s target;
     the city store's 82-610 ms calm-second floor suggests the target is reachable there,
     while the rigs' ~4.5 s floor (dolt + size) suggests it is not, without items 1-2.
