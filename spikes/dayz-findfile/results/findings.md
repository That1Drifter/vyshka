# Findings: FindFile on DayZ 1.30

Measured 2026-10-18 on the Windows dedicated server 1.30.164078, three boots: one with
`-profiles` absolute (`probe-run1.log`, `probe-run2-absolute.log`) and one with it relative
to the server directory (`probe-run3-relative.log`). The profile directory was outside the
server's install directory in every run.

## What was found

- `FindFile` resolves `$profile:` against the server's working directory (its install
  directory), not the `-profiles` directory. `$profile:*` and `$profile:/*` list the
  install directory (`addons`, `dta`, `DayZServer_x64.exe`, ...). `$saves:*` does the same.
- So a directory the script made under `$profile:` cannot be listed. The probe made
  `$profile:VyshkaFind` with three files: `FileExist` and `OpenFile` reached them, and
  every `FindFile` pattern for that directory came back with no handle: `/*.json`,
  `/*`, `\*.json`, the exact file name, and the directory itself. The flag (`DIRECTORIES`,
  `ALL`, `ARCHIVES`) made no difference.
- A path with no alias is refused when it leaves the working directory: the `-profiles`
  value from `CommandlineGetParam`, whether absolute (`C:\...\profile/VyshkaFind/*.json`)
  or relative (`..\..\...\profile/...`), gave no handle, and `OpenFile` and `FileExist` on
  it failed as well. A relative pattern inside the working directory (`mpmissions/*`)
  lists.

## What it means for the plugin

`VyshkaOutbox.Load` lists `$profile:Vyshka/outbox/*.json` to restore unacked envelopes.
On 1.30, with a `-profiles` directory other than the working directory, it finds nothing,
so every envelope still unacked at a restart is left on disk and never sent. The selftest's
`files.outbox` check fails this way (`restored=false loadMs=0`). On 1.29 the same check
passed with the same profile layout, so this is a change in 1.30.

Not measured: the Linux server, and a `-profiles` directory inside the working directory.

## Re-run on 1.30.164289: fixed

Measured 2026-11-04 on the Windows dedicated server 1.30.164289 (the stable hotfix of
2026-11-03, scripts revision 127855), two boots: `-profiles` absolute
(`probe-run4-absolute-130-164289.log`) and relative (`probe-run5-relative-130-164289.log`).

- `FindFile` now resolves `$profile:` against the `-profiles` directory. Every pattern for
  `$profile:VyshkaFind` lists the probe's files (`/*.json` 2, `/*` 3, `\*.json` 2, the exact
  name 1, the directory itself 1), whatever the flag. `$profile:*` lists the profile
  directory, and `$saves:*` lists the saves directory (`DayZ.cfg`) rather than the install
  directory.
- Unchanged: a path with no alias that leaves the working directory is still refused, and
  `mpmissions/*` still lists.
- The selftest on the same build reports `files.outboxRing listing=true pastFound=true`,
  so the outbox's listing fallback is exercised again.
- The Linux server on 1.30.164289 (staging2, `-profiles=/opt/dayz/profiles` outside
  `/opt/dayz/server`) lists too: the branch plugin's first boot found the five records
  plugin 0.9.0 had named past 10 000 (393148 to 393152), which only a listing reaches, and
  delivered them. Linux was not measured on 164078, so whether it ever failed there is
  unknown. The boot's log lines are kept outside the repository with the pass's notes.

So the 1.30 behavior above belongs to 1.30.164078 (and possibly 1.30.164200, which was not
measured). The outbox's ring naming stays, since a server still on 164078 needs it.
