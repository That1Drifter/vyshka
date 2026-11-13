# Spike: how Reforger's string functions index multi-byte text

The Reforger plugin shares its JSON and text code with the DayZ plugin, which reads strings
byte by byte. This checks that Reforger 1.9's `string` functions do the same on text that
holds multi-byte UTF-8, which nothing the plugin conformance suite sends exercises.

Measured 2026-11-13 on the Arma Reforger dedicated server 1.9.0.37 (Windows 11), one boot,
the probe loaded the development way (`-server` world, `-addonsDir`, `-addons
E6B73A7967C84448`). Raw lines: `results/run1.log`.

## Findings

With `"a" + "é" + "b" + "中" + "c"` (é is two bytes, C3 A9; 中 is three, E4 B8 AD):

| Call | Result |
|---|---|
| `Length()` | Bytes: 2 for é, 3 for 中, 8 for the whole string |
| `ToAscii(i)` | The byte at index i, **signed**: C3 reads -61, A9 reads -87, E4 reads -28; ASCII reads 0 to 127 |
| `Get(i)` | A one-byte string holding byte i (its `ToAscii()` is the same signed value) |
| `Substring(1, 2)` | The two bytes of é, equal to `"é"`; `Substring(1, 1)` is one byte |
| `IndexOf` | A byte offset (`b` at 3, `c` at 7) |
| `AsciiToString` | One byte: `195.AsciiToString() + 169.AsciiToString()` equals `"é"` |

So every index is a byte index, and a byte at or above 0x80 reads negative through
`ToAscii`. Code that tests a byte's high bits adds 256 first (`VyshkaText.IsContinuation`
in the plugin does).

## Reproducing

```
ArmaReforgerServer.exe -server worlds/GameMaster/GM_Arland.ent -addonsDir <repo>\spikes\reforger-string-bytes\addon -addons E6B73A7967C84448 -profile <scratch> -nothrow
findstr VYSHKA_BYTES <scratch>\logs\<timestamp>\console.log
```

Run from the server's install folder. The probe prints and does nothing else.
