# Chernarus satellite prototype

An offline basemap experiment for issue #46. It converts the installed DayZ satellite textures into a calibrated raster and static web tiles, with a standalone inspection viewer. It does not connect to the hub or display live players; the panel's live map does that, and reads the dataset this builds.

## Installing the dataset on a hub

The panel's map view (`panel/README.md`, "Map tilesets") reads the same `manifest.json` this builder emits. Copy the manifest and the tile pyramid, and nothing else, into the hub's maps directory under the world id the DayZ plugin reports:

```powershell
$maps = "C:\path\to\maps"
New-Item -ItemType Directory -Force "$maps\chernarusplus" | Out-Null
Copy-Item .scratch\chernarus-satellite\manifest.json "$maps\chernarusplus\"
Copy-Item -Recurse .scratch\chernarus-satellite\tiles "$maps\chernarusplus\tiles"
.\bin\vyshka-hub serve -maps-dir $maps
```

The hub serves only `manifest.json` and `tiles/` from a world directory, so the master raster and the decoded sources never leave the machine even if copied along. The panel reads the manifest's `world`, `bounds`, `raster`, and `tiles` members and ignores the build provenance; the default axes (`x` east, `z` north) are DayZ's.

## Run from the repository root

Prerequisites: Python 3.11 or later, the packages in `requirements.txt`, and local DayZ and DayZ Tools installations. The current run uses Python 3.14, Pillow 12.3.0, and NumPy 2.4.3. Python compatibility beyond that environment has not been exercised.

```powershell
python -m pip install -r spikes/chernarus-satellite/requirements.txt
python spikes/chernarus-satellite/build.py
python spikes/chernarus-satellite/serve.py
```

Open <http://127.0.0.1:8766/>. The viewer supports mouse/touch dragging, zoom controls, keyboard navigation, inspection destinations, and a marker at a supplied world coordinate. The preview server binds only to loopback and serves the viewer and generated dataset, with directory listings disabled.

For a preview that stays available independently of an agent turn, double-click
`Start-Preview.cmd` in this directory and keep its window open. The launcher resolves the
viewer and generated-data paths relative to itself, so it works from Explorer without
setting a working directory. Closing that window stops the server; the map files remain.

The default builder reads:

- `C:\Program Files (x86)\Steam\steamapps\common\DayZ\Addons\worlds_chernarusplus_data.pbo`
- `C:\Program Files (x86)\Steam\steamapps\common\DayZ Tools\Bin\ImageToPAA\ImageToPAA.exe`

Override them with `--pbo` and `--tool`; set `--output` for another build directory, `--workers` for bounded conversion/encoding concurrency, and `--webp-quality` for a compression experiment. The server accepts `--data` and `--port`. Use a new output directory for another completed build, and stop its viewer server before modifying any dataset.

## Outputs

Generated game assets stay in ignored `.scratch/chernarus-satellite/`:

| Path | Purpose |
|---|---|
| `manifest.json` | Dataset contract, source hashes/build, transform, crop, levels, timings, storage, and validation limits |
| `chernarusplus-satellite-master.png` | Lossless 15,360×15,360 reference raster |
| `overview.png` | 1,920×1,920 overview |
| `tiles/{z}/{x}/{y}.webp` | 4,810 tiles across zooms 0–6, at default quality 90 |
| `seam-audit.json` | Comparison of all 1,984 adjacent source-image overlaps |
| `qa/` | Native-resolution inspection crops |
| `source-paa/`, `decoded-png/` | Extracted and decoded intermediates retained for inspection |

Only selected `layers/S_XXX_YYY_lco.paa` members are extracted. The PBO directory and trailer checksum, exact 32×32 source grid, decoded dimensions, and every neighboring 32-pixel RGB overlap must pass before the final manifest is emitted. A mismatch is reported rather than smoothed away.

## Coordinate and image contract

Source tiles are 512×512 with a shared 32-pixel overlap. Cropping 16 pixels on each side gives a 480×480 cell. First filename index runs east; second runs south. Six independently decoded terrain material transforms, including all four corner cells, agree with this registration:

```
world extent: x = 0..15360, z = 0..15360 metres
raster edge coordinates: u = x, v = 15360 - z
pixel centre (i+0.5,j+0.5): x = i+0.5, z = 15360-j-0.5
```

Native zoom 6 is one metre per image pixel. At zoom `z`, raster dimensions are `15360 * 2^(z-6)` and tile columns/rows are the ceiling of that dimension divided by 256. The raster is not stretched to the logical 16,384-pixel tile square. Partial edge tiles contain transparent padding. Lower levels are resampled from the continuous master before tiling to avoid independent resampling boundaries.

The viewer uses DayZ's horizontal x/z axes; elevation y is not plotted. Marker positions are continuous world coordinates. The outer boundary is an edge, not an extra row of source pixels. Inspection destinations taken from terrain location labels are approximate area anchors, not surveyed control points.

## Validation

```powershell
python -m unittest discover -s spikes/chernarus-satellite -p "test_build.py"
node --test spikes/chernarus-satellite/viewer/map-math.test.mjs
```

These checks cover parsing/corruption, crop placement, pyramid dimensions/padding, and viewer coordinate math. The local build additionally checks the real game archive and source seams. Browser inspection must cover loaded imagery, native and reduced zoom, coordinate controls, map bounds, mobile layout, and visible errors/recovery.

### Measured prototype run, 2026-09-12

- Source Steam build: `24689949`; archive SHA-256: `cb437fee51b6a09f7caa67464afd5882c94661307cd5ef9d7a63f2a6565e8504`.
- Full build with four workers: **76.370 seconds**. All **1,984 source overlaps** matched exactly across 97,517,568 RGB channel comparisons.
- **4,810 valid WebP tiles**, totalling **70,733,758 bytes (67.46 MiB)**. Lossless master: 278,429,042 bytes. Whole retained dataset: 934,755,778 bytes (about 891.45 MiB).
- **Eight Python and nine JavaScript tests passed.** The real browser check used Edge at its normal desktop viewport and a 390×844 mobile layout. Loaded imagery, fractional/native zoom, dragging, decimal-coordinate marker placement, southeast boundary, and missing-manifest recovery were exercised. Thin fractional-zoom tile cracks found in the initial browser pass were corrected with shared device-pixel tile edges.
- Native 320×320 crops across Chernogorsk, Novodmitrovsk, Zelenogorsk, Berezino, and the main airfield were compared with decoded web tiles. RGB mean absolute error ranged 1.66–2.29 on a 0–255 scale, with PSNR 38.59–40.64 dB. Visual inspection found minor smoothing/compression differences; the reference PNG remains available for further comparison. These are sample quality measurements, not surveyed location errors.
- The viewer's mobile arrangement and mouse drag were checked at a mobile viewport; physical touch and pinch gestures were not tested. High-DPI level selection has math coverage; physical high-DPI display behaviour was not separately measured.

Detailed local evidence is retained in `.scratch/chernarus-registration-audit/`, including material transforms, a source/WebP comparison sheet, quality metrics, and `prototype-validation.md`. The dataset manifest preserves build-time validation limits; subsequent browser/visual checks are recorded separately.

Asset-derived registration is distinct from a live-engine survey. No claim is made that settlement label positions locate a specific ground feature within two metres. Live-player overlay, click-to-action dispatch, and telemetry freshness remain issue #46 work. Satellite textures preserve ground appearance; they do not render every placed object or replace a topographic map with building footprints and readable labels.

Do not commit generated game assets. Distribution permission is not established by this prototype.
