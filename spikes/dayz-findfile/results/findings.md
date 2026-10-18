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
