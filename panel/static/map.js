// Vyshka panel: the map widget behind the live map view.
//
// A tiled raster basemap on a canvas with markers laid over it as DOM
// buttons. The basemap is described by a dataset manifest the hub serves from
// its maps directory (panel/README.md, "Map tilesets"): a world's extent in
// the game's own map frame (protocol section 8.3), the size of the native
// raster, and a tile pyramid over it. Nothing here knows a game: the manifest
// says which components of a position are easting and northing, and the
// widget plots whatever it is handed.
//
// Coordinates. World (x, z) map onto the native raster as
//   u = (x - xmin) / (xmax - xmin) * width
//   v = (zmax - z) / (zmax - zmin) * height
// so north is up. The camera is a raster point (u, v) at the viewport's
// centre and a continuous zoom, where zoom maxZoom draws one raster pixel per
// CSS pixel and each step halves or doubles that. Tiles come from the pyramid
// level nearest the camera's zoom, upscaled past the native level rather than
// refused, because a live map wants to get closer than the imagery allows.
//
// The DOM is built with createElement and text nodes only; marker labels are
// plugin-supplied text.

const OVERZOOM = 2;
const UNDERZOOM = 2;
const MAX_CACHED_TILES = 512;
const DRAG_THRESHOLD_PX = 4;

function clamp(value, minimum, maximum) {
  return Math.min(maximum, Math.max(minimum, value));
}

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === undefined || value === null || value === false) continue;
    if (key === 'class') node.className = value;
    else if (value === true) node.setAttribute(key, '');
    else node.setAttribute(key, String(value));
  }
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

// ---------------------------------------------------------------------------
// Manifest

function finite(value) {
  return typeof value === 'number' && Number.isFinite(value);
}

function positiveInteger(value) {
  return Number.isInteger(value) && value > 0;
}

// validateManifest checks the dataset contract and returns a normalized copy:
// the axes filled in with their defaults, the display name filled in with
// the world id. It throws with a message fit for the page.
export function validateManifest(raw) {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) throw new Error('the manifest is not a JSON object');
  if (raw.schemaVersion !== 1) throw new Error('the manifest has schemaVersion ' + JSON.stringify(raw.schemaVersion) + ', and this panel reads version 1');
  if (typeof raw.world !== 'string' || raw.world === '') throw new Error('the manifest names no world');
  const bounds = raw.bounds;
  if (!bounds || typeof bounds !== 'object' || !['xmin', 'xmax', 'zmin', 'zmax'].every((key) => finite(bounds[key]))) {
    throw new Error('the manifest bounds must carry finite xmin, xmax, zmin, and zmax');
  }
  if (!(bounds.xmin < bounds.xmax) || !(bounds.zmin < bounds.zmax)) throw new Error('the manifest bounds are empty');
  const raster = raw.raster;
  if (!raster || typeof raster !== 'object' || !positiveInteger(raster.width) || !positiveInteger(raster.height)) {
    throw new Error('the manifest raster must carry a positive integer width and height');
  }
  const tiles = raw.tiles;
  if (!tiles || typeof tiles !== 'object' || !positiveInteger(tiles.size)
      || !Number.isInteger(tiles.minZoom) || !Number.isInteger(tiles.maxZoom) || tiles.minZoom > tiles.maxZoom || tiles.minZoom < 0) {
    throw new Error('the manifest tiles must carry a positive size and integer minZoom <= maxZoom');
  }
  if (typeof tiles.urlTemplate !== 'string' || !['{z}', '{x}', '{y}'].every((token) => tiles.urlTemplate.includes(token))) {
    throw new Error('the manifest tiles.urlTemplate must contain {z}, {x}, and {y}');
  }
  if (tiles.scheme !== undefined && tiles.scheme !== 'xyz') throw new Error('the manifest tiles.scheme must be xyz');
  const axes = raw.axes && typeof raw.axes === 'object' ? raw.axes : {};
  const axis = (given, index, name) => {
    const spec = given && typeof given === 'object' ? given : {};
    if (spec.index !== undefined && !(Number.isInteger(spec.index) && spec.index >= 0)) throw new Error('the manifest axes must index a position component');
    if (spec.name !== undefined && typeof spec.name !== 'string') throw new Error('the manifest axis names must be strings');
    return { index: spec.index === undefined ? index : spec.index, name: spec.name === undefined ? name : spec.name };
  };
  const east = axis(axes.east, 0, 'x');
  const north = axis(axes.north, 2, 'z');
  if (east.index === north.index) throw new Error('the manifest axes name the same position component twice');
  return {
    world: raw.world,
    name: typeof raw.name === 'string' && raw.name ? raw.name : raw.world,
    bounds: { xmin: bounds.xmin, xmax: bounds.xmax, zmin: bounds.zmin, zmax: bounds.zmax },
    raster: { width: raster.width, height: raster.height },
    tiles: { size: tiles.size, minZoom: tiles.minZoom, maxZoom: tiles.maxZoom, urlTemplate: tiles.urlTemplate },
    axes: { east, north },
  };
}

// worldPoint reads the easting and northing out of a position array, per the
// manifest's axes. A two-number position is read as [east, north] whatever
// the axes say, the flat form section 8.3 allows.
export function worldPoint(manifest, position) {
  if (!Array.isArray(position)) return null;
  let x;
  let z;
  if (position.length === 2) {
    [x, z] = position;
  } else {
    x = position[manifest.axes.east.index];
    z = position[manifest.axes.north.index];
  }
  if (!finite(x) || !finite(z)) return null;
  return { x, z };
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

function zoomScale(zoom, nativeZoom) {
  return 2 ** (zoom - nativeZoom);
}

function alignToDevicePixel(value, pixelRatio) {
  return Math.round(value * pixelRatio) / pixelRatio;
}

// fitZoom is the zoom at which the whole raster fits the viewport.
export function fitZoom(manifest, width, height) {
  if (!(width > 0) || !(height > 0)) return manifest.tiles.minZoom;
  const scale = Math.min(width / manifest.raster.width, height / manifest.raster.height);
  return clamp(manifest.tiles.maxZoom + Math.log2(scale), manifest.tiles.minZoom - UNDERZOOM, manifest.tiles.maxZoom + OVERZOOM);
}

// ---------------------------------------------------------------------------
// The widget

// createMap builds the widget inside container and returns its controls.
// options.onSelect(key) is called when a marker is activated. The widget is
// inert until setDataset is given a validated manifest and the URL its tile
// template is relative to.
export function createMap(container, options = {}) {
  const canvas = el('canvas', { class: 'map-canvas' });
  const context = canvas.getContext('2d', { alpha: false });
  const markerLayer = el('div', { class: 'map-markers' });
  const stage = el('div', {
    class: 'map-stage', tabindex: '0', role: 'application',
    'aria-label': 'Map. Drag or use the arrow keys to pan; plus, minus, and the wheel zoom; 0 fits the whole map.',
  }, canvas, markerLayer);
  const cursorOut = el('output', { class: 'map-cursor' }, '');
  const scaleBar = el('span', { class: 'map-scale-bar' });
  const scaleLabel = el('span', { class: 'map-scale-label' }, '');
  const tileStatus = el('output', { class: 'map-tiles', 'aria-live': 'polite' }, '');
  const zoomIn = el('button', { type: 'button', class: 'small', 'aria-label': 'Zoom in' }, '+');
  const zoomOut = el('button', { type: 'button', class: 'small', 'aria-label': 'Zoom out' }, '−');
  const fitButton = el('button', { type: 'button', class: 'small' }, 'Fit');
  const hud = el('div', { class: 'map-hud' },
    el('div', { class: 'map-hud-row' }, zoomOut, zoomIn, fitButton),
    el('div', { class: 'map-hud-row map-scale' }, scaleLabel, scaleBar),
    cursorOut, tileStatus);
  container.append(stage, hud);

  const state = {
    manifest: null,
    baseURL: null,
    viewport: { width: 1, height: 1 },
    pixelRatio: 1,
    camera: { u: 0, v: 0, zoom: 0 },
    markers: [],        // [{ key, label, x, z, node }]
    highlighted: null,
    tiles: new Map(),   // key -> { image, status, lastUsed }
    visibleTiles: new Set(),
    tileUse: 0,
    drag: null,
    dragMoved: false,
    frame: 0,
    destroyed: false,
  };

  const nativeZoom = () => state.manifest.tiles.maxZoom;

  const rasterToScreen = (u, v) => {
    const scale = zoomScale(state.camera.zoom, nativeZoom());
    return {
      x: (u - state.camera.u) * scale + state.viewport.width / 2,
      y: (v - state.camera.v) * scale + state.viewport.height / 2,
    };
  };

  const screenToRaster = (x, y) => {
    const scale = zoomScale(state.camera.zoom, nativeZoom());
    return {
      u: state.camera.u + (x - state.viewport.width / 2) / scale,
      v: state.camera.v + (y - state.viewport.height / 2) / scale,
    };
  };

  // The camera stays over the raster: the view may not be dragged into
  // the void beyond the map's edge, and a raster smaller than the viewport
  // sits centred.
  const constrain = () => {
    const manifest = state.manifest;
    if (!manifest) return;
    const scale = zoomScale(state.camera.zoom, nativeZoom());
    const halfWidth = state.viewport.width / (2 * scale);
    const halfHeight = state.viewport.height / (2 * scale);
    const centreU = manifest.raster.width / 2;
    const centreV = manifest.raster.height / 2;
    state.camera.u = halfWidth >= centreU ? centreU : clamp(state.camera.u, halfWidth, manifest.raster.width - halfWidth);
    state.camera.v = halfHeight >= centreV ? centreV : clamp(state.camera.v, halfHeight, manifest.raster.height - halfHeight);
  };

  const schedule = () => {
    if (state.frame || state.destroyed) return;
    state.frame = requestAnimationFrame(() => {
      state.frame = 0;
      draw();
    });
  };

  const setZoom = (zoom, anchorX = state.viewport.width / 2, anchorY = state.viewport.height / 2) => {
    const manifest = state.manifest;
    if (!manifest) return;
    const before = screenToRaster(anchorX, anchorY);
    state.camera.zoom = clamp(zoom, manifest.tiles.minZoom - UNDERZOOM, manifest.tiles.maxZoom + OVERZOOM);
    const scale = zoomScale(state.camera.zoom, nativeZoom());
    state.camera.u = before.u - (anchorX - state.viewport.width / 2) / scale;
    state.camera.v = before.v - (anchorY - state.viewport.height / 2) / scale;
    constrain();
    schedule();
  };

  const fit = () => {
    const manifest = state.manifest;
    if (!manifest) return;
    state.camera = {
      u: manifest.raster.width / 2,
      v: manifest.raster.height / 2,
      zoom: fitZoom(manifest, state.viewport.width, state.viewport.height),
    };
    constrain();
    schedule();
  };

  // --- tiles

  const tileURL = (z, x, y) => {
    const path = state.manifest.tiles.urlTemplate
      .replaceAll('{z}', String(z)).replaceAll('{x}', String(x)).replaceAll('{y}', String(y));
    return new URL(path, state.baseURL).href;
  };

  const getTile = (z, x, y) => {
    const key = z + '/' + x + '/' + y;
    let tile = state.tiles.get(key);
    if (tile) {
      tile.lastUsed = ++state.tileUse;
      return { key, tile };
    }
    const image = new Image();
    tile = { image, status: 'loading', lastUsed: ++state.tileUse };
    state.tiles.set(key, tile);
    image.addEventListener('load', () => { tile.status = 'loaded'; schedule(); });
    image.addEventListener('error', () => { tile.status = 'error'; schedule(); });
    image.src = tileURL(z, x, y);
    return { key, tile };
  };

  const evictTiles = () => {
    const excess = state.tiles.size - MAX_CACHED_TILES;
    if (excess <= 0) return;
    const removable = [...state.tiles.entries()]
      .filter(([key, tile]) => !state.visibleTiles.has(key) && tile.status !== 'loading')
      .sort((a, b) => a[1].lastUsed - b[1].lastUsed)
      .slice(0, excess);
    for (const [key] of removable) state.tiles.delete(key);
  };

  const drawTiles = () => {
    const manifest = state.manifest;
    const { size, minZoom, maxZoom } = manifest.tiles;
    const tileZoom = clamp(Math.round(state.camera.zoom + Math.log2(Math.max(1, state.pixelRatio))), minZoom, maxZoom);
    const levelScale = zoomScale(tileZoom, maxZoom);
    const projected = zoomScale(state.camera.zoom, tileZoom);
    const levelWidth = manifest.raster.width * levelScale;
    const levelHeight = manifest.raster.height * levelScale;
    const maxTileX = Math.ceil(levelWidth / size) - 1;
    const maxTileY = Math.ceil(levelHeight / size) - 1;
    const topLeft = screenToRaster(0, 0);
    const bottomRight = screenToRaster(state.viewport.width, state.viewport.height);
    const left = Math.max(0, topLeft.u);
    const top = Math.max(0, topLeft.v);
    const right = Math.min(manifest.raster.width, bottomRight.u);
    const bottom = Math.min(manifest.raster.height, bottomRight.v);
    const startX = clamp(Math.floor((left * levelScale) / size), 0, maxTileX);
    const endX = clamp(Math.floor((right * levelScale - Number.EPSILON) / size), 0, maxTileX);
    const startY = clamp(Math.floor((top * levelScale) / size), 0, maxTileY);
    const endY = clamp(Math.floor((bottom * levelScale - Number.EPSILON) / size), 0, maxTileY);
    const origin = rasterToScreen(0, 0);
    const visible = new Set();
    context.imageSmoothingEnabled = true;
    context.imageSmoothingQuality = 'high';
    for (let y = startY; y <= endY; y++) {
      for (let x = startX; x <= endX; x++) {
        const { key, tile } = getTile(tileZoom, x, y);
        visible.add(key);
        const sourceWidth = Math.min(size, levelWidth - x * size);
        const sourceHeight = Math.min(size, levelHeight - y * size);
        // Tile edges are shared in device pixels so that neighbours meet
        // without a crack at fractional zooms.
        const screenX = alignToDevicePixel(origin.x + x * size * projected, state.pixelRatio);
        const screenY = alignToDevicePixel(origin.y + y * size * projected, state.pixelRatio);
        const screenRight = alignToDevicePixel(origin.x + (x * size + sourceWidth) * projected, state.pixelRatio);
        const screenBottom = alignToDevicePixel(origin.y + (y * size + sourceHeight) * projected, state.pixelRatio);
        if (tile.status === 'loaded') {
          context.drawImage(tile.image, 0, 0, sourceWidth, sourceHeight, screenX, screenY, screenRight - screenX, screenBottom - screenY);
        } else {
          context.fillStyle = tile.status === 'error' ? '#3a2a26' : '#1c221e';
          context.fillRect(screenX, screenY, screenRight - screenX, screenBottom - screenY);
        }
      }
    }
    state.visibleTiles = visible;
    evictTiles();
  };

  const drawBoundary = () => {
    const manifest = state.manifest;
    const topLeft = rasterToScreen(0, 0);
    const bottomRight = rasterToScreen(manifest.raster.width, manifest.raster.height);
    context.strokeStyle = 'rgba(255, 255, 255, 0.5)';
    context.lineWidth = 1;
    context.strokeRect(Math.round(topLeft.x) + 0.5, Math.round(topLeft.y) + 0.5,
      Math.round(bottomRight.x - topLeft.x), Math.round(bottomRight.y - topLeft.y));
  };

  const placeMarkers = () => {
    const margin = 40;
    for (const marker of state.markers) {
      const raster = worldToRaster(state.manifest, marker.x, marker.z);
      const screen = rasterToScreen(raster.u, raster.v);
      const outside = screen.x < -margin || screen.y < -margin
        || screen.x > state.viewport.width + margin || screen.y > state.viewport.height + margin;
      marker.node.hidden = outside;
      if (!outside) marker.node.style.transform = 'translate(' + screen.x + 'px, ' + screen.y + 'px)';
    }
  };

  const updateScale = () => {
    const manifest = state.manifest;
    const metresPerPixel = ((manifest.bounds.xmax - manifest.bounds.xmin) / manifest.raster.width)
      / zoomScale(state.camera.zoom, nativeZoom());
    const ideal = metresPerPixel * 90;
    const magnitude = 10 ** Math.floor(Math.log10(ideal));
    const normalized = ideal / magnitude;
    const nice = normalized >= 5 ? 5 : normalized >= 2 ? 2 : 1;
    const metres = nice * magnitude;
    scaleBar.style.width = (metres / metresPerPixel) + 'px';
    scaleLabel.textContent = metres >= 1000 ? (metres / 1000) + ' km' : metres + ' m';
  };

  const updateTileStatus = () => {
    let loaded = 0;
    let failed = 0;
    for (const key of state.visibleTiles) {
      const status = state.tiles.get(key)?.status;
      if (status === 'loaded') loaded++;
      else if (status === 'error') failed++;
    }
    const total = state.visibleTiles.size;
    tileStatus.textContent = loaded + '/' + total + ' tiles' + (failed ? ', ' + failed + ' failed' : '');
    tileStatus.dataset.loaded = String(loaded);
    tileStatus.dataset.total = String(total);
    tileStatus.dataset.failed = String(failed);
  };

  const draw = () => {
    if (state.destroyed) return;
    context.fillStyle = '#121513';
    context.fillRect(0, 0, state.viewport.width, state.viewport.height);
    if (!state.manifest) return;
    drawTiles();
    drawBoundary();
    placeMarkers();
    updateScale();
    updateTileStatus();
  };

  const resize = () => {
    const rect = stage.getBoundingClientRect();
    const width = Math.max(1, Math.round(rect.width));
    const height = Math.max(1, Math.round(rect.height));
    const ratio = Math.max(1, window.devicePixelRatio || 1);
    const first = state.viewport.width === 1 && state.viewport.height === 1;
    state.viewport = { width, height };
    state.pixelRatio = ratio;
    canvas.width = Math.round(width * ratio);
    canvas.height = Math.round(height * ratio);
    context.setTransform(ratio, 0, 0, ratio, 0, 0);
    // The first real size fits the map; later sizes keep the view.
    if (first && state.manifest) fit();
    constrain();
    schedule();
  };

  // --- interaction

  const pointerPoint = (event) => {
    const rect = stage.getBoundingClientRect();
    return { x: event.clientX - rect.left, y: event.clientY - rect.top };
  };

  const isMarker = (target) => target instanceof Element && target.closest('.map-marker') !== null;

  stage.addEventListener('pointerdown', (event) => {
    if (!state.manifest || event.button !== 0) return;
    // A drag that starts on a marker is not captured, so that a press and
    // release on it still reaches the marker as a click; dragMoved tells
    // the click apart from a pan that happened to begin there.
    if (!isMarker(event.target)) stage.setPointerCapture(event.pointerId);
    state.drag = { pointerId: event.pointerId, x: event.clientX, y: event.clientY };
    state.dragMoved = false;
    stage.classList.add('dragging');
  });

  stage.addEventListener('pointermove', (event) => {
    if (state.manifest) {
      const point = pointerPoint(event);
      const raster = screenToRaster(point.x, point.y);
      const world = rasterToWorld(state.manifest, raster.u, raster.v);
      const bounds = state.manifest.bounds;
      const inside = world.x >= bounds.xmin && world.x <= bounds.xmax && world.z >= bounds.zmin && world.z <= bounds.zmax;
      cursorOut.textContent = inside
        ? state.manifest.axes.east.name + ' ' + Math.round(world.x) + '  ' + state.manifest.axes.north.name + ' ' + Math.round(world.z)
        : '';
    }
    if (!state.drag || state.drag.pointerId !== event.pointerId) return;
    const dx = event.clientX - state.drag.x;
    const dy = event.clientY - state.drag.y;
    if (!state.dragMoved && Math.hypot(dx, dy) < DRAG_THRESHOLD_PX) return;
    state.dragMoved = true;
    const scale = zoomScale(state.camera.zoom, nativeZoom());
    state.camera.u -= dx / scale;
    state.camera.v -= dy / scale;
    state.drag.x = event.clientX;
    state.drag.y = event.clientY;
    constrain();
    schedule();
  });

  const endDrag = (event) => {
    if (!state.drag || state.drag.pointerId !== event.pointerId) return;
    state.drag = null;
    stage.classList.remove('dragging');
  };
  stage.addEventListener('pointerup', endDrag);
  stage.addEventListener('pointercancel', endDrag);
  stage.addEventListener('pointerleave', () => { cursorOut.textContent = ''; });

  stage.addEventListener('wheel', (event) => {
    if (!state.manifest) return;
    event.preventDefault();
    const point = pointerPoint(event);
    setZoom(state.camera.zoom - event.deltaY * 0.0015, point.x, point.y);
  }, { passive: false });

  stage.addEventListener('keydown', (event) => {
    if (!state.manifest || event.target !== stage) return;
    const step = (event.shiftKey ? 180 : 70) / zoomScale(state.camera.zoom, nativeZoom());
    switch (event.key) {
      case 'ArrowLeft': state.camera.u -= step; break;
      case 'ArrowRight': state.camera.u += step; break;
      case 'ArrowUp': state.camera.v -= step; break;
      case 'ArrowDown': state.camera.v += step; break;
      case '+': case '=': setZoom(state.camera.zoom + 0.5); return;
      case '-': case '_': setZoom(state.camera.zoom - 0.5); return;
      case '0': fit(); return;
      default: return;
    }
    event.preventDefault();
    constrain();
    schedule();
  });

  zoomIn.addEventListener('click', () => setZoom(state.camera.zoom + 0.5));
  zoomOut.addEventListener('click', () => setZoom(state.camera.zoom - 0.5));
  fitButton.addEventListener('click', fit);

  const observer = new ResizeObserver(resize);
  observer.observe(stage);
  resize();

  return {
    // setDataset gives the widget a validated manifest and the URL its tile
    // template resolves against, and fits the view to it.
    setDataset(manifest, baseURL) {
      state.manifest = manifest;
      state.baseURL = baseURL;
      state.tiles.clear();
      state.visibleTiles = new Set();
      stage.setAttribute('aria-label', manifest.name + ' map. Drag or use the arrow keys to pan; plus, minus, and the wheel zoom; 0 fits the whole map.');
      fit();
    },
    // setMarkers replaces every marker. Each is { key, label, position };
    // one whose position the manifest cannot read is not plotted, and the
    // caller decides what to say about it. Nodes are keyed and reused so
    // a marker under the pointer survives a refresh.
    setMarkers(markers) {
      const manifest = state.manifest;
      if (!manifest) return;
      const existing = new Map(state.markers.map((marker) => [marker.key, marker]));
      const next = [];
      for (const given of markers) {
        const point = worldPoint(manifest, given.position);
        if (!point) continue;
        let marker = existing.get(given.key);
        if (marker) {
          existing.delete(given.key);
        } else {
          const node = el('button', { type: 'button', class: 'map-marker', 'data-marker-key': given.key },
            el('span', { class: 'map-marker-dot' }), el('span', { class: 'map-marker-label' }, ''));
          node.addEventListener('click', () => {
            if (state.dragMoved) return;
            if (typeof options.onSelect === 'function') options.onSelect(given.key);
          });
          marker = { key: given.key, node };
          markerLayer.append(node);
        }
        marker.x = point.x;
        marker.z = point.z;
        marker.label = given.label;
        marker.node.querySelector('.map-marker-label').textContent = given.label;
        marker.node.title = given.label + ' (' + manifest.axes.east.name + ' ' + Math.round(point.x) + ', ' + manifest.axes.north.name + ' ' + Math.round(point.z) + ')';
        marker.node.dataset.x = String(point.x);
        marker.node.dataset.z = String(point.z);
        marker.node.classList.toggle('highlighted', given.key === state.highlighted);
        next.push(marker);
      }
      for (const stale of existing.values()) stale.node.remove();
      state.markers = next;
      schedule();
    },
    // setHighlight emphasizes one marker (or none with null).
    setHighlight(key) {
      state.highlighted = key;
      for (const marker of state.markers) marker.node.classList.toggle('highlighted', marker.key === key);
    },
    // focus centres the view on a marker, zooming in to at least the
    // native level.
    focus(key) {
      const marker = state.markers.find((entry) => entry.key === key);
      if (!marker || !state.manifest) return false;
      const raster = worldToRaster(state.manifest, marker.x, marker.z);
      state.camera.u = raster.u;
      state.camera.v = raster.v;
      state.camera.zoom = Math.max(state.camera.zoom, nativeZoom());
      constrain();
      schedule();
      return true;
    },
    fit,
    destroy() {
      state.destroyed = true;
      observer.disconnect();
      if (state.frame) cancelAnimationFrame(state.frame);
      state.tiles.clear();
      state.markers = [];
    },
  };
}
