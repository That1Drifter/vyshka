# Vyshka sample mod

A complete DayZ server mod written against the Vyshka plugin's surface, small enough to read
in one sitting and meant to be copied. It depends on nothing but the game: `requiredAddons`
names `DZ_Data` and `DZ_Scripts` and nothing else, and every file that names a Vyshka class
sits behind `#ifdef VYSHKA`, so the same PBO also loads on a server that is not running the
plugin (it says so in the log and registers nothing).

## What it shows

Each piece of the surface once, in `scripts/4_World/VyshkaSample/SampleBeacon.c` and
`scripts/5_Mission/VyshkaSample/SampleMission.c`:

- **Registration.** `MissionServer.VyshkaRegister` is overridden, calls `super` first, and
  registers everything below, so it is all in the first manifest the server publishes.
- **A key/value namespace.** `registry.DeclareNamespace("sample")`, without which every
  store call the mod makes is refused before it reaches the hub.
- **A custom context.** `sample.landmark` ("Landmark") with two entries, Green Mountain and
  the Northwest Airfield. The hub asks for them with `context.enumerate` and offers them as
  the reference keys of any action in that context.
- **An action.** `sample.beacon` ("Place a beacon", danger `none`, one optional `label`
  string) takes the landmark as its `referenceKey`.
- **A map marker.** The action places `sample:beacon:<landmark>`, which rides the
  `state.entities` snapshot onto the panel's live map.
- **A telemetry event.** `sample.beacon.placed`, declared with a payload schema and emitted
  through `GetVyshka().Emit`.
- **The key/value store.** The action counts placements under `sample/beacons.<landmark>`
  with a guarded write (read the revision, write only if nobody else has since, one retry
  on a mismatch), returns `Pending()` while the store answers, and completes with the count.
  The beacon is placed whatever the store does: the result carries `stored` and, when that
  is false, `storeError`.

## Building

```
go run ./plugins/dayz/cmd/vyshka-dayz build-sample
```

writes `plugins/dayz/build/@VyshkaSample/addons/VyshkaSample.pbo` and a `mod.cpp`, the same
way `build` packs the plugin itself. Nothing but Go is needed.

## Loading

```
DayZServer_x64.exe -config=serverDZ.cfg -profiles=<profiles> -serverMod=@Vyshka;@VyshkaSample -dologs
```

`@Vyshka` must come first. The engine defines a mod's `defines[]` symbols only for the mods
it loads afterwards, so a `@VyshkaSample` loaded ahead of the plugin compiles without
`VYSHKA`, registers nothing, and logs that it did.

With both loaded, the server's script log carries
`[VyshkaSample] registered with Vyshka <version>, link <state>` at boot, the manifest the
hub stores carries the action, the context, and the event, and a dispatch places a beacon:

```
curl -X POST https://hub.example.net/api/v1/servers/<serverId>/actions \
  -H "Authorization: Bearer <admin token>" -H "Content-Type: application/json" \
  -d '{"code":"sample.beacon","context":"sample.landmark","referenceKey":"green-mountain","params":{}}'
```

## Making it yours

Rename the classes, the namespace, the context id, and the action code; the file names and
the `dir`, `name`, and `files[]` entries in `config.cpp` go with them. Keep the `#ifdef
VYSHKA` guards and the `super.VyshkaRegister(registry)` call. The surface itself is
documented in `plugins/dayz/README.md` under "Writing a mod against the plugin".
