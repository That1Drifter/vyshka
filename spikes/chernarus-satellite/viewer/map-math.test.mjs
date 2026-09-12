import test from "node:test";
import assert from "node:assert/strict";

import {
  alignToDevicePixel,
  clampCamera,
  chooseTileEvictions,
  chooseTileZoom,
  displayRasterSize,
  fitCameraZoom,
  hasVerifiedRegistration,
  rasterToScreen,
  rasterToWorld,
  screenToRaster,
  validateManifest,
  worldToRaster,
} from "./map-math.mjs";

const manifest = {
  schemaVersion: 1,
  world: "chernarusplus",
  bounds: { xmin: 0, xmax: 15360, zmin: 0, zmax: 15360 },
  raster: { width: 15360, height: 15360, metresPerPixel: 1 },
  tiles: {
    size: 256,
    minZoom: 0,
    maxZoom: 6,
    urlTemplate: "tiles/{z}/{x}/{y}.webp",
    format: "webp",
    scheme: "xyz",
  },
};

test("validates the exact prototype manifest contract", () => {
  assert.equal(validateManifest(structuredClone(manifest)).world, "chernarusplus");
  assert.throws(
    () => validateManifest({ ...manifest, tiles: { ...manifest.tiles, scheme: "tms" } }),
    /tiles\.scheme must be "xyz"/,
  );
  assert.throws(
    () => validateManifest({ ...manifest, tiles: { ...manifest.tiles, urlTemplate: "one.webp" } }),
    /must contain \{z\}, \{x\}, and \{y\}/,
  );
});

test("registration status requires measured evidence tied to the loaded archive", () => {
  assert.equal(hasVerifiedRegistration(manifest), false);
  const measuredOnly = {
    ...manifest,
    validation: { measured: { assetMaterialRegistration: true } },
  };
  assert.equal(hasVerifiedRegistration(measuredOnly), false);
  assert.equal(hasVerifiedRegistration({
    ...measuredOnly,
    transform: { calibrationEvidence: { appliesToCurrentArchive: true } },
  }), true);
});

test("maps world north to raster top and round-trips real Chernarus bounds", () => {
  assert.deepEqual(worldToRaster(manifest, 0, 15360), { u: 0, v: 0 });
  assert.deepEqual(worldToRaster(manifest, 15360, 0), { u: 15360, v: 15360 });
  assert.deepEqual(worldToRaster(manifest, 4600, 10400), { u: 4600, v: 4960 });
  assert.deepEqual(rasterToWorld(manifest, 4600, 4960), { x: 4600, z: 10400 });
});

test("uses a 15360 pixel native level without stretching to a power of two", () => {
  assert.deepEqual(displayRasterSize(manifest, 6), { width: 15360, height: 15360 });
  assert.deepEqual(displayRasterSize(manifest, 0), { width: 240, height: 240 });
});

test("screen and raster transforms round-trip at fractional camera zoom", () => {
  const viewport = { width: 911, height: 637 };
  const camera = { u: 6033.25, v: 8199.75, zoom: 4.35 };
  const screen = rasterToScreen(camera, viewport, 7200, 4500, manifest.tiles.maxZoom);
  const raster = screenToRaster(camera, viewport, screen.x, screen.y, manifest.tiles.maxZoom);
  assert.ok(Math.abs(raster.u - 7200) < 1e-9);
  assert.ok(Math.abs(raster.v - 4500) < 1e-9);
});

test("adjacent fractional tile edges share one backing-pixel boundary", () => {
  const dpr = 1.5;
  const origin = -103.271;
  const tileWidth = 197.337;
  const firstRight = alignToDevicePixel(origin + tileWidth, dpr);
  const secondLeft = alignToDevicePixel(origin + 1 * tileWidth, dpr);
  const secondRight = alignToDevicePixel(origin + 2 * tileWidth, dpr);
  assert.equal(firstRight, secondLeft);
  assert.equal(firstRight * dpr, Math.round(firstRight * dpr));
  assert.equal(secondRight * dpr, Math.round(secondRight * dpr));
});

test("fit zoom keeps the full map visible and selects a real tile level", () => {
  const zoom = fitCameraZoom(manifest, 960, 720);
  assert.ok(Math.abs(zoom - (6 + Math.log2(720 / 15360))) < 1e-12);
  assert.equal(chooseTileZoom(zoom, 0, 6), 2);
  assert.equal(chooseTileZoom(5.6, 0, 6), 6);
  assert.equal(chooseTileZoom(-4, 0, 6), 0);
  assert.equal(chooseTileZoom(4.3, 0, 6, 2), 5);
  assert.equal(chooseTileZoom(5.4, 0, 6, 3), 6);
});

test("tile eviction is least-recently-used while preserving visible and loading tiles", () => {
  const entries = [
    ["old", { status: "loaded", lastUsed: 1 }],
    ["loading", { status: "loading", lastUsed: 2 }],
    ["visible", { status: "loaded", lastUsed: 3 }],
    ["failed", { status: "error", lastUsed: 4 }],
    ["recent", { status: "loaded", lastUsed: 5 }],
  ];
  assert.deepEqual(
    chooseTileEvictions(entries, new Set(["visible"]), 3),
    ["old", "failed"],
  );
  assert.deepEqual(chooseTileEvictions(entries, new Set(["visible"]), 10), []);
});

test("camera constraints retain the map in the viewport", () => {
  const viewport = { width: 1000, height: 600 };
  const highZoom = clampCamera({ u: -2000, v: 99999, zoom: 6 }, viewport, manifest);
  assert.deepEqual(highZoom, { u: 500, v: 15060, zoom: 6 });

  const overview = clampCamera({ u: 0, v: 0, zoom: 0 }, viewport, manifest);
  assert.deepEqual(overview, { u: 7680, v: 7680, zoom: 0 });
});
