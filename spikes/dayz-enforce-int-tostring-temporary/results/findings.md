# DayZ `int.ToString()` inside a concatenation: measurements

Resolves issue #50 (envelope id prefix `dz-90911T...` for 2026-09-11).

**Short answer:** the result of `int.ToString()` is not materialized when it is evaluated.
While it is still pending in an expression, it ends up holding the value of the last
`int.ToString()` run inside any function the same expression calls afterwards.
`year.ToString() + Pad2(month) + Pad2(day)` therefore reads as `"9" + "09" + "11"` on
2026-09-11: the year slot took the value of the last `ToString()` that `Pad2` ran before the
year was consumed. Copying the result into a local first, or concatenating a string literal
onto it before the call, materializes it and the expression is correct. The mechanism named
here is an inference from the measured outcomes below, not a documented language rule.

## Environment

| Item | Value |
|---|---|
| Game build | DayZ 1.29.163709, server binary `DayZServer_x64.exe` |
| Script defines | `DAYZ_1_29, SERVER_FOR_WINDOWS, SERVER, PLATFORM_WINDOWS, RELEASE, NO_GUI` |
| Host OS | Windows 11 Pro 26100 |
| Harness | `harness/VyshkaToStringProbe.c` in the `init.c` of the spike mission from `../dayz-restapi-poll-timeout` |
| Date | 2026-09-12 |

The probe is a loose mission script, not a packed mod. The defect was first observed in the
packed plugin (issue #50), so both compilation paths show it.

## Results

Inputs are the literals `year = 2026`, `month = 9`, `day = 11` unless the case says
`live-clock`, where they come from `GetYearMonthDayUTC` on 2026-09-12. `Pad2(v)` is the
plugin's helper: `"0" + v.ToString()` under 10, `v.ToString()` otherwise. Full log:
`probe-script.log`.

| Case | Expression | Got | Want | OK |
|---|---|---|---|---|
| direct-chain | `year.ToString() + Pad2(month) + Pad2(day)` | `90911` | `20260911` | no |
| literal-between | `year.ToString() + "-" + Pad2(month) + "-" + Pad2(day)` | `2026-09-11` | `2026-09-11` | yes |
| two-tostring | `year.ToString() + month.ToString()` | `20269` | `20269` | yes |
| three-tostring | `year.ToString() + month.ToString() + day.ToString()` | `2026911` | `2026911` | yes |
| tostring-then-plain-call | `year.ToString() + Echo("x")` | `2026x` | `2026x` | yes |
| tostring-then-stringify | `year.ToString() + Stringify(day)` where `Stringify` returns `day.ToString()` | `1111` | `202611` | no |
| tostring-then-pad-low | `year.ToString() + Pad2(month)` | `909` | `202609` | no |
| tostring-then-pad-high | `year.ToString() + Pad2(day)` | `1111` | `202611` | no |
| tostring-then-int-call | `year.ToString() + Twice(day)` where `Twice` returns an int | `202622` | `202622` | yes |
| tostring-then-float-call | `year.ToString() + FloatStr(1.5)` where `FloatStr` returns `f.ToString()` | `20261.5` | `20261.5` | yes |
| float-tostring-then-pad | `f.ToString() + Pad2(month)` with `f = 1.5` | `1.509` | `1.509` | yes |
| call-then-tostring | `Pad2(month) + year.ToString()` | `092026` | `092026` | yes |
| tostring-as-argument | `Concat(year.ToString(), Pad2(month))` | `909` | `202609` | no |
| parenthesized | `year.ToString() + (Pad2(month) + Pad2(day))` | `110911` | `20260911` | no |
| local-copy | `string y = year.ToString(); y + Pad2(month) + Pad2(day)` | `20260911` | `20260911` | yes |
| append-assign | `s = year.ToString(); s += Pad2(month); s += Pad2(day)` | `20260911` | `20260911` | yes |
| implicit-int-concat | `"" + year + Pad2(month) + Pad2(day)` | `20260911` | `20260911` | yes |
| format | `string.Format("%1%2%3", year, Pad2(month), Pad2(day))` | `20260911` | `20260911` | yes |
| live-clock-direct-chain | the plugin's expression over the real clock | `90912` | `20260912` | no |
| live-clock-local-copy | the fix over the real clock | `20260912` | `20260912` | yes |

## Reading the results

- **The pending result is replaced by the callee's last `int.ToString()`.** Every failing
  case shows the year slot holding exactly the value the called function converted last:
  `9` from `Pad2(9)`, `11` from `Pad2(11)` or `Stringify(11)`, and `11` in the
  parenthesized case where `Pad2(day)` ran last inside the group.
- **Same-function `ToString()` calls do not interfere.** Two and three conversions in one
  expression with no function call between them are correct, so the pending result is not a
  single global slot; only a nested call overwrites it.
- **Only a `ToString()` in the callee does it.** A callee that returns a string without
  converting (`Echo`) or returns an int (`Twice`) leaves the pending result alone.
- **It is per type.** A float conversion inside the callee does not disturb a pending int
  conversion, and an int conversion inside the callee does not disturb a pending float one.
- **Order matters.** With the call first and the `ToString()` last there is nothing pending
  when the callee runs, so `Pad2(month) + year.ToString()` is correct.
- **Passing the pending result as an argument does not protect it.** Evaluating a later
  argument that calls a converting function still replaces it.
- **A literal concatenated first materializes it.** That is the only reason the RFC 3339
  formatter was correct while the compact one was not.

## What the plugin does with this

`VyshkaClock.NowCompact` and `NowRfc3339` copy the year into a local before calling `Pad2`
(the local-copy shape, measured correct against literals and the live clock). No other
concatenation in the plugin has a pending `int.ToString()` followed by a call into a
converting function: at every other site the conversion is consumed into a string (a
literal, an accumulated concatenation, an assignment, or `+=`) before any later call, or it
is the final operand. The probe does not run the complete formatters themselves; the rebuilt
plugin did, under the plugin conformance harness on 2026-09-12, and minted
`dz-20260912T115305-7c690e6d-15` with a correct `ts` beside it.

Ids already minted with the short prefix stay valid: an envelope id is opaque to the hub,
which compares ids as whole strings and accepts up to 128 bytes. Uniqueness rests where it
did before, on the per-boot timestamp and random tag plus the counter, which is
probabilistic across restarts in the same way the ULID recommendation of spec section 4 is;
the probe measures none of that. The prefix is minted once per process, so the corrected
shape appears from the next server boot running the fixed plugin.

Behavior can change between game patches. Re-run the spike when the plugin is validated
against a new major DayZ version.
