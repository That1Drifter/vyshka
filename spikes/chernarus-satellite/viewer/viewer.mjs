import {
  alignToDevicePixel,
  clamp,
  clampCamera,
  chooseTileEvictions,
  chooseTileZoom,
  fitCameraZoom,
  hasVerifiedRegistration,
  rasterToScreen,
  rasterToWorld,
  screenToRaster,
  validateManifest,
  worldToRaster,
  zoomScale,
} from "./map-math.mjs";

const canvas = document.querySelector("#map-canvas");
const context = canvas.getContext("2d", { alpha: false });
const mapPanel = document.querySelector(".map-panel");
const mapMessage = document.querySelector("#map-message");
const registrationNote = document.querySelector("#registration-note");
const loadStatus = document.querySelector("#load-status");
const cursorCoordinate = document.querySelector("#cursor-coordinate");
const scaleBar = document.querySelector("#scale-bar");
const scaleLabel = document.querySelector("#scale-label");
const coordinateForm = document.querySelector("#coordinate-form");
const coordinateX = document.querySelector("#coordinate-x");
const coordinateZ = document.querySelector("#coordinate-z");
const coordinateError = document.querySelector("#coordinate-error");
const manifestForm = document.querySelector("#manifest-form");
const manifestInput = document.querySelector("#manifest-url");

const state = {
  viewport: { width: 1, height: 1 },
  pixelRatio: 1,
  manifest: null,
  manifestURL: null,
  camera: { u: 7680, v: 7680, zoom: 0 },
  marker: null,
  tileCache: new Map(),
  visibleTileKeys: new Set(),
  drag: null,
  loadGeneration: 0,
  tileUseCounter: 0,
};

const MAX_CACHED_TILES = 512;

const initialManifest = new URLSearchParams(location.search).get("manifest") || "/data/manifest.json";
manifestInput.value = initialManifest;

function showMessage(message) {
  mapMessage.textContent = message;
  mapMessage.hidden = false;
}

function clearMessage() {
  mapMessage.textContent = "";
  mapMessage.hidden = true;
}

function formatError(error) {
  return error instanceof Error ? error.message : String(error);
}

function updateRegistrationNote(manifest) {
  registrationNote.textContent = hasVerifiedRegistration(manifest)
    ? "Local asset viewer · asset coordinate transform verified for this archive. Landmark and live-player alignment remain untested."
    : "Local asset viewer · asset registration pending verification. Landmark and live-player alignment remain untested.";
}

async function loadManifest(inputURL) {
  const generation = ++state.loadGeneration;
  state.manifest = null;
  state.tileCache.clear();
  updateRegistrationNote(null);
  loadStatus.textContent = "Loading manifest…";
  clearMessage();
  draw();

  try {
    const requestedURL = new URL(inputURL, location.href);
    const response = await fetch(requestedURL, { headers: { Accept: "application/json" } });
    if (!response.ok) {
      throw new Error(`Manifest request failed: HTTP ${response.status} ${response.statusText}`.trim());
    }
    const manifest = validateManifest(await response.json());
    if (generation !== state.loadGeneration) return;

    state.manifest = manifest;
    state.manifestURL = new URL(response.url || requestedURL.href);
    updateRegistrationNote(manifest);
    fitMap();
  } catch (error) {
    if (generation !== state.loadGeneration) return;
    const message = `Unable to load the satellite dataset. ${formatError(error)}`;
    loadStatus.textContent = "Dataset unavailable";
    showMessage(message);
    draw();
  }
}

function fitMap() {
  if (!state.manifest) return;
  state.camera = {
    u: state.manifest.raster.width / 2,
    v: state.manifest.raster.height / 2,
    zoom: fitCameraZoom(state.manifest, state.viewport.width, state.viewport.height),
  };
  constrainCamera();
  draw();
}

function constrainCamera() {
  if (!state.manifest) return;
  state.camera = clampCamera(state.camera, state.viewport, state.manifest);
}

function setZoom(nextZoom, anchorX = state.viewport.width / 2, anchorY = state.viewport.height / 2) {
  if (!state.manifest) return;
  const nativeZoom = state.manifest.tiles.maxZoom;
  const before = screenToRaster(state.camera, state.viewport, anchorX, anchorY, nativeZoom);
  state.camera.zoom = clamp(nextZoom, state.manifest.tiles.minZoom, nativeZoom);
  const nextScale = zoomScale(state.camera.zoom, nativeZoom);
  state.camera.u = before.u - (anchorX - state.viewport.width / 2) / nextScale;
  state.camera.v = before.v - (anchorY - state.viewport.height / 2) / nextScale;
  constrainCamera();
  draw();
}

function tileURL(z, x, y) {
  const path = state.manifest.tiles.urlTemplate
    .replaceAll("{z}", String(z))
    .replaceAll("{x}", String(x))
    .replaceAll("{y}", String(y));
  return new URL(path, state.manifestURL).href;
}

function getTile(z, x, y) {
  const key = `${z}/${x}/${y}`;
  let tile = state.tileCache.get(key);
  if (tile) {
    tile.lastUsed = ++state.tileUseCounter;
    return { key, tile };
  }

  const image = new Image();
  tile = {
    image,
    status: "loading",
    url: tileURL(z, x, y),
    lastUsed: ++state.tileUseCounter,
  };
  state.tileCache.set(key, tile);
  image.addEventListener("load", () => {
    tile.status = "loaded";
    draw();
  });
  image.addEventListener("error", () => {
    tile.status = "error";
    draw();
  });
  image.src = tile.url;
  return { key, tile };
}

function evictOldTiles() {
  const keys = chooseTileEvictions(
    [...state.tileCache.entries()],
    state.visibleTileKeys,
    MAX_CACHED_TILES,
  );
  for (const key of keys) state.tileCache.delete(key);
}

function drawBackdrop() {
  context.fillStyle = "#090c0a";
  context.fillRect(0, 0, state.viewport.width, state.viewport.height);
  const grid = 24;
  context.fillStyle = "#0d120e";
  for (let y = 0; y < state.viewport.height; y += grid) {
    for (let x = (Math.floor(y / grid) % 2) * grid; x < state.viewport.width; x += grid * 2) {
      context.fillRect(x, y, grid, grid);
    }
  }
}

function drawTilePlaceholder(x, y, width, height, failed) {
  context.fillStyle = failed ? "#301f1a" : "#172019";
  context.fillRect(x, y, width, height);
  context.strokeStyle = failed ? "#7f5043" : "#28342b";
  context.strokeRect(x + 0.5, y + 0.5, width - 1, height - 1);
  if (failed && width > 90 && height > 54) {
    context.fillStyle = "#d5aaa0";
    context.font = "12px system-ui, sans-serif";
    context.fillText("tile unavailable", x + 10, y + 22);
  }
}

function visibleRasterBounds() {
  const nativeZoom = state.manifest.tiles.maxZoom;
  const topLeft = screenToRaster(state.camera, state.viewport, 0, 0, nativeZoom);
  const bottomRight = screenToRaster(
    state.camera,
    state.viewport,
    state.viewport.width,
    state.viewport.height,
    nativeZoom,
  );
  return {
    left: Math.max(0, topLeft.u),
    top: Math.max(0, topLeft.v),
    right: Math.min(state.manifest.raster.width, bottomRight.u),
    bottom: Math.min(state.manifest.raster.height, bottomRight.v),
  };
}

function drawTiles() {
  const manifest = state.manifest;
  const tileZoom = chooseTileZoom(
    state.camera.zoom,
    manifest.tiles.minZoom,
    manifest.tiles.maxZoom,
    state.pixelRatio,
  );
  const levelScale = zoomScale(tileZoom, manifest.tiles.maxZoom);
  const projectedTileSize = manifest.tiles.size * zoomScale(state.camera.zoom, tileZoom);
  const levelWidth = manifest.raster.width * levelScale;
  const levelHeight = manifest.raster.height * levelScale;
  const maxTileX = Math.ceil(levelWidth / manifest.tiles.size) - 1;
  const maxTileY = Math.ceil(levelHeight / manifest.tiles.size) - 1;
  const bounds = visibleRasterBounds();
  const startX = clamp(Math.floor((bounds.left * levelScale) / manifest.tiles.size), 0, maxTileX);
  const endX = clamp(Math.floor(((bounds.right * levelScale) - Number.EPSILON) / manifest.tiles.size), 0, maxTileX);
  const startY = clamp(Math.floor((bounds.top * levelScale) / manifest.tiles.size), 0, maxTileY);
  const endY = clamp(Math.floor(((bounds.bottom * levelScale) - Number.EPSILON) / manifest.tiles.size), 0, maxTileY);
  const origin = rasterToScreen(state.camera, state.viewport, 0, 0, manifest.tiles.maxZoom);
  const currentVisible = new Set();

  context.imageSmoothingEnabled = true;
  context.imageSmoothingQuality = "high";
  for (let y = startY; y <= endY; y += 1) {
    for (let x = startX; x <= endX; x += 1) {
      const { key, tile } = getTile(tileZoom, x, y);
      currentVisible.add(key);
      const sourceWidth = Math.min(manifest.tiles.size, levelWidth - x * manifest.tiles.size);
      const sourceHeight = Math.min(manifest.tiles.size, levelHeight - y * manifest.tiles.size);
      const screenX = alignToDevicePixel(
        origin.x + x * projectedTileSize,
        state.pixelRatio,
      );
      const screenY = alignToDevicePixel(
        origin.y + y * projectedTileSize,
        state.pixelRatio,
      );
      const screenRight = alignToDevicePixel(
        origin.x + (x * manifest.tiles.size + sourceWidth) * zoomScale(state.camera.zoom, tileZoom),
        state.pixelRatio,
      );
      const screenBottom = alignToDevicePixel(
        origin.y + (y * manifest.tiles.size + sourceHeight) * zoomScale(state.camera.zoom, tileZoom),
        state.pixelRatio,
      );
      const drawWidth = screenRight - screenX;
      const drawHeight = screenBottom - screenY;

      if (tile.status === "loaded") {
        context.drawImage(tile.image, 0, 0, sourceWidth, sourceHeight, screenX, screenY, drawWidth, drawHeight);
      } else {
        drawTilePlaceholder(screenX, screenY, drawWidth, drawHeight, tile.status === "error");
      }
    }
  }
  state.visibleTileKeys = currentVisible;
  evictOldTiles();
}

function drawMapBoundary() {
  const manifest = state.manifest;
  const topLeft = rasterToScreen(state.camera, state.viewport, 0, 0, manifest.tiles.maxZoom);
  const bottomRight = rasterToScreen(
    state.camera,
    state.viewport,
    manifest.raster.width,
    manifest.raster.height,
    manifest.tiles.maxZoom,
  );
  context.strokeStyle = "rgba(235, 224, 191, 0.72)";
  context.lineWidth = 1;
  context.strokeRect(
    Math.round(topLeft.x) + 0.5,
    Math.round(topLeft.y) + 0.5,
    Math.round(bottomRight.x - topLeft.x),
    Math.round(bottomRight.y - topLeft.y),
  );
}

function drawMarker() {
  if (!state.marker || !state.manifest) return;
  const raster = worldToRaster(state.manifest, state.marker.x, state.marker.z);
  const screen = rasterToScreen(
    state.camera,
    state.viewport,
    raster.u,
    raster.v,
    state.manifest.tiles.maxZoom,
  );
  if (screen.x < -30 || screen.y < -30 || screen.x > state.viewport.width + 30 || screen.y > state.viewport.height + 30) return;

  context.save();
  context.translate(screen.x, screen.y);
  context.beginPath();
  context.arc(0, 0, 10, 0, Math.PI * 2);
  context.fillStyle = "rgba(13, 17, 14, 0.78)";
  context.fill();
  context.lineWidth = 3;
  context.strokeStyle = "#ffd166";
  context.stroke();
  context.beginPath();
  context.moveTo(-15, 0);
  context.lineTo(15, 0);
  context.moveTo(0, -15);
  context.lineTo(0, 15);
  context.lineWidth = 2;
  context.strokeStyle = "#fff5d6";
  context.stroke();
  context.font = "600 12px system-ui, sans-serif";
  context.textBaseline = "middle";
  const label = `x ${Math.round(state.marker.x)} · z ${Math.round(state.marker.z)}`;
  const labelWidth = context.measureText(label).width + 14;
  const labelX = 19;
  context.fillStyle = "rgba(10, 14, 11, 0.88)";
  context.fillRect(labelX, -12, labelWidth, 24);
  context.fillStyle = "#fff5d6";
  context.fillText(label, labelX + 7, 0);
  context.restore();
}

function updateScale() {
  if (!state.manifest) {
    scaleLabel.textContent = "-";
    scaleBar.style.width = "5rem";
    return;
  }
  const metresPerScreenPixel = state.manifest.raster.metresPerPixel
    / zoomScale(state.camera.zoom, state.manifest.tiles.maxZoom);
  const idealMetres = metresPerScreenPixel * 90;
  const magnitude = 10 ** Math.floor(Math.log10(idealMetres));
  const normalized = idealMetres / magnitude;
  const nice = normalized >= 5 ? 5 : normalized >= 2 ? 2 : 1;
  const metres = nice * magnitude;
  scaleBar.style.width = `${metres / metresPerScreenPixel}px`;
  scaleLabel.textContent = metres >= 1000 ? `${metres / 1000} km` : `${metres} m`;
}

function updateLoadStatus() {
  if (!state.manifest) return;
  let loading = 0;
  let errors = 0;
  let loaded = 0;
  for (const key of state.visibleTileKeys) {
    const status = state.tileCache.get(key)?.status;
    if (status === "loaded") loaded += 1;
    else if (status === "error") errors += 1;
    else loading += 1;
  }
  const zoom = state.camera.zoom.toFixed(2);
  const tileZoom = chooseTileZoom(
    state.camera.zoom,
    state.manifest.tiles.minZoom,
    state.manifest.tiles.maxZoom,
    state.pixelRatio,
  );
  const parts = [`View ${zoom} · tiles z${tileZoom}`, `${loaded}/${state.visibleTileKeys.size} visible loaded`];
  if (loading) parts.push(`${loading} loading`);
  if (errors) parts.push(`${errors} failed`);
  loadStatus.textContent = parts.join(" · ");
  if (errors) {
    showMessage(`${errors} visible map tile${errors === 1 ? "" : "s"} could not be loaded. Check the dataset path and server output.`);
  } else if (!loading) {
    clearMessage();
  }
}

function draw() {
  drawBackdrop();
  if (state.manifest) {
    drawTiles();
    drawMapBoundary();
    drawMarker();
  }
  updateScale();
  updateLoadStatus();
}

function resizeCanvas() {
  const rect = mapPanel.getBoundingClientRect();
  const width = Math.max(1, Math.round(rect.width));
  const height = Math.max(1, Math.round(rect.height));
  const dpr = Math.max(1, window.devicePixelRatio || 1);
  state.viewport = { width, height };
  state.pixelRatio = dpr;
  canvas.width = Math.round(width * dpr);
  canvas.height = Math.round(height * dpr);
  context.setTransform(dpr, 0, 0, dpr, 0, 0);
  constrainCamera();
  draw();
}

function eventPoint(event) {
  const rect = canvas.getBoundingClientRect();
  return { x: event.clientX - rect.left, y: event.clientY - rect.top };
}

canvas.addEventListener("pointerdown", (event) => {
  if (!state.manifest || event.button !== 0) return;
  canvas.setPointerCapture(event.pointerId);
  state.drag = { pointerId: event.pointerId, x: event.clientX, y: event.clientY };
  canvas.classList.add("dragging");
});

canvas.addEventListener("pointermove", (event) => {
  const point = eventPoint(event);
  if (state.manifest) {
    const raster = screenToRaster(
      state.camera,
      state.viewport,
      point.x,
      point.y,
      state.manifest.tiles.maxZoom,
    );
    const world = rasterToWorld(state.manifest, raster.u, raster.v);
    const inside = world.x >= state.manifest.bounds.xmin && world.x <= state.manifest.bounds.xmax
      && world.z >= state.manifest.bounds.zmin && world.z <= state.manifest.bounds.zmax;
    cursorCoordinate.textContent = inside
      ? `x ${Math.round(world.x)} · z ${Math.round(world.z)}`
      : "outside map";
  }
  if (!state.drag || state.drag.pointerId !== event.pointerId) return;

  const scale = zoomScale(state.camera.zoom, state.manifest.tiles.maxZoom);
  state.camera.u -= (event.clientX - state.drag.x) / scale;
  state.camera.v -= (event.clientY - state.drag.y) / scale;
  state.drag.x = event.clientX;
  state.drag.y = event.clientY;
  constrainCamera();
  draw();
});

function endDrag(event) {
  if (!state.drag || state.drag.pointerId !== event.pointerId) return;
  state.drag = null;
  canvas.classList.remove("dragging");
}

canvas.addEventListener("pointerup", endDrag);
canvas.addEventListener("pointercancel", endDrag);
canvas.addEventListener("pointerleave", () => {
  cursorCoordinate.textContent = "x - · z -";
});

canvas.addEventListener("wheel", (event) => {
  if (!state.manifest) return;
  event.preventDefault();
  const point = eventPoint(event);
  setZoom(state.camera.zoom - event.deltaY * 0.0015, point.x, point.y);
}, { passive: false });

canvas.addEventListener("keydown", (event) => {
  if (!state.manifest) return;
  const panPixels = event.shiftKey ? 180 : 70;
  const scale = zoomScale(state.camera.zoom, state.manifest.tiles.maxZoom);
  if (event.key === "ArrowLeft") state.camera.u -= panPixels / scale;
  else if (event.key === "ArrowRight") state.camera.u += panPixels / scale;
  else if (event.key === "ArrowUp") state.camera.v -= panPixels / scale;
  else if (event.key === "ArrowDown") state.camera.v += panPixels / scale;
  else if (event.key === "+" || event.key === "=") setZoom(state.camera.zoom + 0.5);
  else if (event.key === "-" || event.key === "_") setZoom(state.camera.zoom - 0.5);
  else if (event.key === "0") fitMap();
  else return;
  event.preventDefault();
  constrainCamera();
  draw();
});

document.querySelector("#zoom-in").addEventListener("click", () => setZoom(state.camera.zoom + 0.5));
document.querySelector("#zoom-out").addEventListener("click", () => setZoom(state.camera.zoom - 0.5));
document.querySelector("#fit-map").addEventListener("click", fitMap);
document.querySelector("#preset-overview").addEventListener("click", fitMap);
document.querySelector("#go-inspection").addEventListener("click", () => {
  if (!state.manifest) return;
  const selection = document.querySelector("#inspection-area");
  if (!selection.value) {
    selection.focus();
    return;
  }
  const [x, z] = selection.value.split(",").map(Number);
  const raster = worldToRaster(state.manifest, x, z);
  state.camera = { u: raster.u, v: raster.v, zoom: 4.5 };
  constrainCamera();
  draw();
  loadStatus.textContent = `${selection.options[selection.selectedIndex].text} inspection area · approximate label anchor`;
});

coordinateForm.addEventListener("submit", (event) => {
  event.preventDefault();
  if (!state.manifest) {
    coordinateError.textContent = "Load a valid dataset first.";
    return;
  }
  const rawX = coordinateX.value.trim();
  const rawZ = coordinateZ.value.trim();
  const x = Number(rawX);
  const z = Number(rawZ);
  const bounds = state.manifest.bounds;
  if (!rawX || !rawZ || !Number.isFinite(x) || !Number.isFinite(z)
      || x < bounds.xmin || x > bounds.xmax || z < bounds.zmin || z > bounds.zmax) {
    coordinateError.textContent = `Enter x and z from ${bounds.xmin} through ${bounds.xmax} metres.`;
    return;
  }
  coordinateError.textContent = "";
  state.marker = { x, z };
  const raster = worldToRaster(state.manifest, x, z);
  state.camera.u = raster.u;
  state.camera.v = raster.v;
  state.camera.zoom = Math.max(state.camera.zoom, 4);
  constrainCamera();
  draw();
});

document.querySelector("#clear-marker").addEventListener("click", () => {
  state.marker = null;
  coordinateError.textContent = "";
  draw();
});

manifestForm.addEventListener("submit", (event) => {
  event.preventDefault();
  const inputURL = manifestInput.value.trim();
  if (!inputURL) {
    showMessage("Enter a manifest URL.");
    return;
  }
  const nextPageURL = new URL(location.href);
  if (inputURL === "/data/manifest.json") nextPageURL.searchParams.delete("manifest");
  else nextPageURL.searchParams.set("manifest", inputURL);
  history.replaceState(null, "", nextPageURL);
  loadManifest(inputURL);
});

new ResizeObserver(resizeCanvas).observe(mapPanel);
resizeCanvas();
loadManifest(initialManifest);
