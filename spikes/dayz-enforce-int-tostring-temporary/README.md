# Spike: DayZ `int.ToString()` inside a concatenation that calls another `ToString()`

Resolves issue #50: the DayZ plugin minted envelope ids like `dz-90911T145913-...` for
2026-09-11 while the RFC 3339 `ts` on the same record was right. Both come from the same
`year.ToString() + Pad2(month) + ...` pattern in `VyshkaClock`; the only difference is a
string literal between the year and the first `Pad2()` call in the working one. This spike
pins down what the engine does with the expression and which rewrite is safe.

Findings: [`results/findings.md`](results/findings.md).

## Layout

| Path | What it is |
|---|---|
| `harness/VyshkaToStringProbe.c` | Enforce Script probe: a matrix of string expressions over fixed ints, one tab-separated line per case with the value the engine produced and the value expected. |
| `results/` | Captured server script log and the write-up. |

## Reproducing

1. Reuse the spike mission from `../dayz-restapi-poll-timeout` (its README describes making
   it): replace the probe appended to its `init.c` with `harness/VyshkaToStringProbe.c` and
   call `VyshkaToStringProbe.Run();` as the last statement of `main()`.

2. Start the server as in that README, with `-profiles=<this spike>/results/profile`.

3. Read the results with:

   ```
   findstr VYSHKA_TSPROBE <spike>\results\profile\script_*.log
   ```

The matrix is synchronous and finishes during mission init. No stub server, no game client,
and no mod PBO are needed.

## Provenance

Clean-room: the probe and the measurements are original work written from the public engine
script headers shipped with the game and from observed behavior. No third-party integration
product was consulted or inspected.
