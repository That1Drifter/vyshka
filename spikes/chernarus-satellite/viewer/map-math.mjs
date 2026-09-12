export function clamp(value, minimum, maximum) {
  return Math.min(maximum, Math.max(minimum, value));
}

export function worldToRaster(manifest, x, z) {
  const { xmin, xmax, zmin, zmax } = manifest.bounds;
  return {
    u: ((x - xmin) / (xmax - xmin)) * manifest.raster.width,
    v: ((zmax - z) / (zmax - zmin)) * manifest.raster.height,
  };
}

export function rasterToWorld(manifest, u, v) {
  const { xmin, xmax, zmin, zmax } = manifest.bounds;
  return {
    x: xmin + (u / manifest.raster.width) * (xmax - xmin),
    z: zmax - (v / manifest.raster.height) * (zmax - zmin),
  };
}

export function zoomScale(zoom, nativeZoom) {
  return 2 ** (zoom - nativeZoom);
}

export function displayRasterSize(manifest, zoom) {
  const scale = zoomScale(zoom, manifest.tiles.maxZoom);
  return {
    width: manifest.raster.width * scale,
    height: manifest.raster.height * scale,
  };
}

export function chooseTileZoom(cameraZoom, minZoom, maxZoom, pixelRatio = 1) {
  const densityOffset = Math.log2(Math.max(1, pixelRatio));
  return clamp(Math.round(cameraZoom + densityOffset), minZoom, maxZoom);
}

export function chooseTileEvictions(tileEntries, visibleKeys, maximumEntries) {
  const removable = tileEntries
    .filter(([key, tile]) => !visibleKeys.has(key) && tile.status !== "loading")
    .sort((left, right) => left[1].lastUsed - right[1].lastUsed);
  const removalCount = Math.min(
    removable.length,
    Math.max(0, tileEntries.length - maximumEntries),
  );
  return removable.slice(0, removalCount).map(([key]) => key);
}

export function alignToDevicePixel(value, pixelRatio = 1) {
  const density = Math.max(1, pixelRatio);
  return Math.round(value * density) / density;
}

export function hasVerifiedRegistration(manifest) {
  return manifest?.validation?.measured?.assetMaterialRegistration === true
    && manifest?.transform?.calibrationEvidence?.appliesToCurrentArchive === true;
}

export function fitCameraZoom(manifest, viewportWidth, viewportHeight) {
  if (!(viewportWidth > 0) || !(viewportHeight > 0)) {
    return manifest.tiles.minZoom;
  }
  const fitScale = Math.min(
    viewportWidth / manifest.raster.width,
    viewportHeight / manifest.raster.height,
  );
  return clamp(
    manifest.tiles.maxZoom + Math.log2(fitScale),
    manifest.tiles.minZoom,
    manifest.tiles.maxZoom,
  );
}

export function rasterToScreen(camera, viewport, u, v, nativeZoom) {
  const scale = zoomScale(camera.zoom, nativeZoom);
  return {
    x: (u - camera.u) * scale + viewport.width / 2,
    y: (v - camera.v) * scale + viewport.height / 2,
  };
}

export function screenToRaster(camera, viewport, x, y, nativeZoom) {
  const scale = zoomScale(camera.zoom, nativeZoom);
  return {
    u: camera.u + (x - viewport.width / 2) / scale,
    v: camera.v + (y - viewport.height / 2) / scale,
  };
}

export function clampCamera(camera, viewport, manifest) {
  const scale = zoomScale(camera.zoom, manifest.tiles.maxZoom);
  const halfWidth = viewport.width / (2 * scale);
  const halfHeight = viewport.height / (2 * scale);
  const centreU = manifest.raster.width / 2;
  const centreV = manifest.raster.height / 2;

  return {
    ...camera,
    u: halfWidth >= centreU
      ? centreU
      : clamp(camera.u, halfWidth, manifest.raster.width - halfWidth),
    v: halfHeight >= centreV
      ? centreV
      : clamp(camera.v, halfHeight, manifest.raster.height - halfHeight),
  };
}

export function validateManifest(manifest) {
  const expected = {
    schemaVersion: 1,
    world: "chernarusplus",
    bounds: { xmin: 0, xmax: 15360, zmin: 0, zmax: 15360 },
    raster: { width: 15360, height: 15360, metresPerPixel: 1 },
    tiles: { size: 256, minZoom: 0, maxZoom: 6, format: "webp", scheme: "xyz" },
  };

  if (!manifest || typeof manifest !== "object") {
    throw new Error("Manifest must be a JSON object.");
  }
  if (manifest.schemaVersion !== expected.schemaVersion) {
    throw new Error("Unsupported manifest schema; expected schemaVersion 1.");
  }
  if (manifest.world !== expected.world) {
    throw new Error('This prototype accepts only world "chernarusplus".');
  }

  for (const [groupName, group] of Object.entries({
    bounds: expected.bounds,
    raster: expected.raster,
    tiles: expected.tiles,
  })) {
    const actualGroup = manifest[groupName];
    if (!actualGroup || typeof actualGroup !== "object") {
      throw new Error(`Manifest is missing ${groupName}.`);
    }
    for (const [field, value] of Object.entries(group)) {
      if (actualGroup[field] !== value) {
        throw new Error(`Manifest ${groupName}.${field} must be ${JSON.stringify(value)}.`);
      }
    }
  }

  if (typeof manifest.tiles.urlTemplate !== "string"
      || !["{z}", "{x}", "{y}"].every((token) => manifest.tiles.urlTemplate.includes(token))) {
    throw new Error("Manifest tiles.urlTemplate must contain {z}, {x}, and {y}.");
  }
  return manifest;
}
