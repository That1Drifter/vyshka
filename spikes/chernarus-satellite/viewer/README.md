# Chernarus satellite inspection viewer

This is a dependency-free local viewer for the prototype tile pyramid. It is an asset and
registration inspection tool, not a production panel or a live-player map. The viewer reports
the asset transform as verified only when the manifest ties measured registration evidence to
its current archive; landmark and live-player alignment remain untested.

## Serve locally

Keep generated assets in the ignored scratch directory. From the repository root, run:

```powershell
python spikes/chernarus-satellite/serve.py
```

Then open <http://127.0.0.1:8766/>. The default manifest URL is
`/data/manifest.json`. It can be changed in the viewer or supplied as a query parameter:

```text
http://127.0.0.1:8766/?manifest=/another/path/manifest.json
```

Tile URLs are resolved relative to the final manifest response URL. Loading the HTML directly
from disk is unsupported because browsers restrict `fetch` from `file:` pages.

## Manifest contract

The viewer intentionally accepts only the measured Chernarus prototype contract:

```json
{
  "schemaVersion": 1,
  "world": "chernarusplus",
  "bounds": { "xmin": 0, "xmax": 15360, "zmin": 0, "zmax": 15360 },
  "raster": { "width": 15360, "height": 15360, "metresPerPixel": 1 },
  "tiles": {
    "size": 256,
    "minZoom": 0,
    "maxZoom": 6,
    "urlTemplate": "tiles/{z}/{x}/{y}.webp",
    "format": "webp",
    "scheme": "xyz"
  }
}
```

Zoom 6 is the 15,360 × 15,360 native raster at one metre per pixel. Zoom 0 is 240 × 240.
The raster is not stretched to 16,384 pixels; partial web tiles are transparently padded. North-origin XYZ rows follow
`u = x` and `v = 15360 - z` for these bounds.

## Test

Run the coordinate, camera, and zoom checks with Node.js 20 or newer:

```powershell
node --test spikes/chernarus-satellite/viewer/map-math.test.mjs
```
