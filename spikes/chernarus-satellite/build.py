#!/usr/bin/env python3
"""Build a calibrated Chernarus satellite mosaic and local XYZ tile pyramid.

This spike reads the PBO directory itself and extracts only the expected
layers/S_XXX_YYY_lco.paa members.  It intentionally has no dependency on a
general-purpose PBO unpacker.
"""

from __future__ import annotations

import argparse
import atexit
import concurrent.futures
import hashlib
import json
import math
import os
import re
import shutil
import struct
import subprocess
import sys
import time
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import BinaryIO, Iterable

import numpy as np
from PIL import Image


VERSION_METHOD = 0x56657273
ENTRY_FIELDS = struct.Struct("<IIIII")
MAX_HEADER_STRING = 16 * 1024
GRID_SIZE = 32
SOURCE_SIZE = 512
OVERLAP = 32
BORDER = 16
CELL_SIZE = SOURCE_SIZE - OVERLAP
MOSAIC_SIZE = GRID_SIZE * CELL_SIZE
TILE_SIZE = 256
MIN_ZOOM = 0
MAX_ZOOM = 6
MEMBER_RE = re.compile(r"^layers/S_(\d{3})_(\d{3})_lco\.paa$", re.ASCII)
REGISTRATION_AUDITED_PBO_SHA256 = (
    "cb437fee51b6a09f7caa67464afd5882c94661307cd5ef9d7a63f2a6565e8504"
)
EXPECTED_PBO_PREFIX = r"DZ\worlds\chernarusplus\data"

DEFAULT_PBO = Path(
    r"C:\Program Files (x86)\Steam\steamapps\common\DayZ"
    r"\Addons\worlds_chernarusplus_data.pbo"
)
DEFAULT_TOOL = Path(
    r"C:\Program Files (x86)\Steam\steamapps\common\DayZ Tools"
    r"\Bin\ImageToPAA\ImageToPAA.exe"
)
DEFAULT_OUTPUT = Path(".scratch/chernarus-satellite")


class BuildError(RuntimeError):
    """A validation or build operation failed."""


@dataclass(frozen=True)
class PBOEntry:
    name: str
    method: int
    original_size: int
    reserved: int
    timestamp: int
    data_size: int
    offset: int = 0


@dataclass(frozen=True)
class PBOIndex:
    extensions: dict[str, str]
    entries: tuple[PBOEntry, ...]
    header_size: int
    payload_size: int
    trailer_sha1: str


def _read_cstring(stream: BinaryIO, *, limit: int = MAX_HEADER_STRING) -> bytes:
    value = bytearray()
    for _ in range(limit + 1):
        byte = stream.read(1)
        if not byte:
            raise BuildError("PBO header ended inside a NUL-terminated string")
        if byte == b"\x00":
            return bytes(value)
        value.extend(byte)
    raise BuildError(f"PBO header string exceeds {limit} bytes")


def _read_entry(stream: BinaryIO) -> tuple[bytes, tuple[int, int, int, int, int]]:
    name = _read_cstring(stream)
    raw = stream.read(ENTRY_FIELDS.size)
    if len(raw) != ENTRY_FIELDS.size:
        raise BuildError("PBO header ended inside an entry")
    return name, ENTRY_FIELDS.unpack(raw)


def read_pbo_index(path: Path, *, verify_checksum: bool = True) -> PBOIndex:
    """Read and validate a PBO directory without materializing file bodies."""
    file_size = path.stat().st_size
    if file_size < 42:
        raise BuildError("PBO is too short to contain a directory and SHA-1 trailer")

    entries: list[PBOEntry] = []
    extensions: dict[str, str] = {}
    with path.open("rb") as stream:
        first_name, first_fields = _read_entry(stream)
        if first_name or first_fields[0] != VERSION_METHOD or any(first_fields[1:]):
            raise BuildError("PBO does not begin with the expected version entry")

        while True:
            key_raw = _read_cstring(stream)
            if not key_raw:
                break
            value_raw = _read_cstring(stream)
            try:
                key = key_raw.decode("utf-8")
                value = value_raw.decode("utf-8")
            except UnicodeDecodeError as exc:
                raise BuildError("PBO extension is not valid UTF-8") from exc
            if key in extensions:
                raise BuildError(f"PBO repeats extension key {key!r}")
            extensions[key] = value

        seen_names: set[str] = set()
        while True:
            name_raw, fields = _read_entry(stream)
            if not name_raw:
                if any(fields):
                    raise BuildError("PBO terminator entry has non-zero fields")
                break
            try:
                name = name_raw.decode("utf-8")
            except UnicodeDecodeError as exc:
                raise BuildError("PBO member name is not valid UTF-8") from exc
            if name in seen_names:
                raise BuildError(f"PBO repeats member name {name!r}")
            seen_names.add(name)
            entries.append(PBOEntry(name, *fields))

        header_size = stream.tell()
        payload_size = file_size - 21
        body_size = sum(entry.data_size for entry in entries)
        if header_size + body_size != payload_size:
            raise BuildError(
                "PBO directory sizes do not end at the SHA-1 trailer: "
                f"header={header_size}, bodies={body_size}, payload={payload_size}"
            )

        offset = header_size
        indexed: list[PBOEntry] = []
        for entry in entries:
            indexed.append(PBOEntry(**{**asdict(entry), "offset": offset}))
            offset += entry.data_size

        stream.seek(payload_size)
        trailer = stream.read(21)
        if len(trailer) != 21 or trailer[0] != 0:
            raise BuildError("PBO has an invalid SHA-1 trailer")
        trailer_sha1 = trailer[1:].hex()

    if verify_checksum:
        digest = hashlib.sha1()
        with path.open("rb") as stream:
            remaining = payload_size
            while remaining:
                chunk = stream.read(min(8 * 1024 * 1024, remaining))
                if not chunk:
                    raise BuildError("PBO ended while calculating its payload SHA-1")
                digest.update(chunk)
                remaining -= len(chunk)
        if digest.hexdigest() != trailer_sha1:
            raise BuildError("PBO payload SHA-1 does not match its trailer")

    return PBOIndex(
        extensions=extensions,
        entries=tuple(indexed),
        header_size=header_size,
        payload_size=payload_size,
        trailer_sha1=trailer_sha1,
    )


def normalized_member_name(name: str) -> str:
    normalized = name.replace("\\", "/")
    parts = normalized.split("/")
    if (
        normalized.startswith("/")
        or re.match(r"^[A-Za-z]:/", normalized)
        or any(part in ("", ".", "..") for part in parts)
    ):
        raise BuildError(f"unsafe PBO member name {name!r}")
    return normalized


def validate_source_identity(index: PBOIndex) -> None:
    if index.extensions.get("product") != "dayz":
        raise BuildError(
            f"unexpected PBO product {index.extensions.get('product')!r}; expected 'dayz'"
        )
    if index.extensions.get("prefix") != EXPECTED_PBO_PREFIX:
        raise BuildError(
            f"unexpected PBO prefix {index.extensions.get('prefix')!r}; "
            f"expected {EXPECTED_PBO_PREFIX!r}"
        )


def select_satellite_entries(index: PBOIndex) -> dict[tuple[int, int], PBOEntry]:
    selected: dict[tuple[int, int], PBOEntry] = {}
    for entry in index.entries:
        normalized = normalized_member_name(entry.name)
        match = MEMBER_RE.fullmatch(normalized)
        if not match:
            continue
        x, y = (int(group) for group in match.groups())
        coordinate = (x, y)
        if coordinate in selected:
            raise BuildError(f"duplicate satellite coordinate {coordinate}")
        if entry.method != 0:
            raise BuildError(
                f"satellite member {normalized!r} uses unsupported packing method "
                f"0x{entry.method:08x}"
            )
        if entry.data_size <= 0:
            raise BuildError(f"satellite member {normalized!r} is empty")
        selected[coordinate] = entry

    expected = {(x, y) for y in range(GRID_SIZE) for x in range(GRID_SIZE)}
    missing = sorted(expected - selected.keys())
    extra = sorted(selected.keys() - expected)
    if missing or extra or len(selected) != GRID_SIZE * GRID_SIZE:
        raise BuildError(
            f"satellite grid is not exactly {GRID_SIZE}x{GRID_SIZE}; "
            f"found={len(selected)}, missing={missing[:8]}, extra={extra[:8]}"
        )
    return selected


def _atomic_replace(temp_path: Path, destination: Path) -> None:
    destination.parent.mkdir(parents=True, exist_ok=True)
    os.replace(temp_path, destination)


def extract_selected(
    pbo_path: Path, entries: dict[tuple[int, int], PBOEntry], output_dir: Path
) -> tuple[dict[tuple[int, int], Path], dict[str, str]]:
    output_dir.mkdir(parents=True, exist_ok=True)
    paths: dict[tuple[int, int], Path] = {}
    hashes: dict[str, str] = {}
    with pbo_path.open("rb") as source:
        for coordinate, entry in sorted(entries.items(), key=lambda item: (item[0][1], item[0][0])):
            x, y = coordinate
            destination = output_dir / f"S_{x:03d}_{y:03d}_lco.paa"
            temp_path = destination.with_name(destination.name + ".tmp")
            source.seek(entry.offset)
            remaining = entry.data_size
            digest = hashlib.sha256()
            with temp_path.open("wb") as target:
                while remaining:
                    chunk = source.read(min(1024 * 1024, remaining))
                    if not chunk:
                        raise BuildError(f"PBO ended inside {entry.name!r}")
                    target.write(chunk)
                    digest.update(chunk)
                    remaining -= len(chunk)
            if temp_path.stat().st_size != entry.data_size:
                raise BuildError(f"short extraction for {entry.name!r}")
            _atomic_replace(temp_path, destination)
            # Preserve the archive timestamp so a verified decoded PNG remains
            # reusable on a rerun that extracts the same source again.
            if entry.timestamp:
                os.utime(destination, (entry.timestamp, entry.timestamp))
            paths[coordinate] = destination
            hashes[destination.name] = digest.hexdigest()
    return paths, hashes


def _validate_decoded(path: Path) -> None:
    try:
        with Image.open(path) as image:
            image.load()
            if image.size != (SOURCE_SIZE, SOURCE_SIZE):
                raise BuildError(f"{path.name} decoded to {image.size}, expected 512x512")
            if image.mode not in ("RGB", "RGBA"):
                raise BuildError(f"{path.name} decoded to unexpected mode {image.mode}")
    except (OSError, ValueError) as exc:
        raise BuildError(f"could not validate decoded image {path}") from exc


def _decode_one(tool_path: Path, source: Path, destination: Path) -> None:
    destination.parent.mkdir(parents=True, exist_ok=True)
    temp_path = destination.with_name(destination.stem + ".tmp.png")
    if temp_path.exists():
        temp_path.unlink()
    try:
        result = subprocess.run(
            [str(tool_path), str(source), str(temp_path)],
            capture_output=True,
            text=True,
            errors="replace",
            timeout=120,
            check=False,
        )
    except subprocess.TimeoutExpired as exc:
        raise BuildError(f"ImageToPAA timed out for {source.name}") from exc
    except OSError as exc:
        raise BuildError(f"could not start ImageToPAA for {source.name}: {exc}") from exc
    if result.returncode != 0 or not temp_path.is_file():
        detail = "\n".join(part.strip() for part in (result.stdout, result.stderr) if part.strip())
        raise BuildError(
            f"ImageToPAA failed for {source.name} with exit code {result.returncode}: {detail}"
        )
    _validate_decoded(temp_path)
    _atomic_replace(temp_path, destination)


def decode_all(
    tool_path: Path,
    sources: dict[tuple[int, int], Path],
    output_dir: Path,
    workers: int,
) -> dict[tuple[int, int], Path]:
    if workers < 1 or workers > 16:
        raise BuildError("workers must be between 1 and 16")
    destinations = {
        coordinate: output_dir / (source.stem + ".png")
        for coordinate, source in sources.items()
    }
    # Decode every selected member. A timestamp-only cache can silently reuse
    # stale pixels when an archive changes without a newer member timestamp.
    jobs = [(source, destinations[coordinate]) for coordinate, source in sources.items()]

    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as executor:
        futures = [executor.submit(_decode_one, tool_path, source, destination) for source, destination in jobs]
        for completed, future in enumerate(concurrent.futures.as_completed(futures), 1):
            future.result()
            if completed % 128 == 0 or completed == len(futures):
                print(f"decoded {completed}/{len(futures)} images", flush=True)

    for destination in destinations.values():
        _validate_decoded(destination)
    return destinations


def _rgb_array(path: Path) -> np.ndarray:
    with Image.open(path) as image:
        return np.asarray(image.convert("RGB"), dtype=np.uint8)


def audit_seams(
    images: dict[tuple[int, int], Path], audit_path: Path
) -> dict[str, object]:
    comparisons = 0
    differing_pixels = 0
    differing_channels = 0
    absolute_difference_sum = 0
    maximum_difference = 0
    errors: list[dict[str, object]] = []

    edges: dict[tuple[int, int], tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]] = {}
    for coordinate, path in images.items():
        rgb = _rgb_array(path)
        edges[coordinate] = (
            rgb[:, :OVERLAP].copy(),
            rgb[:, SOURCE_SIZE - OVERLAP :].copy(),
            rgb[:OVERLAP, :].copy(),
            rgb[SOURCE_SIZE - OVERLAP :, :].copy(),
        )

    def compare(
        first_coord: tuple[int, int],
        second_coord: tuple[int, int],
        direction: str,
        first_edge: int,
        second_edge: int,
    ) -> None:
        nonlocal comparisons, differing_pixels, differing_channels
        nonlocal absolute_difference_sum, maximum_difference
        first = edges[first_coord][first_edge].astype(np.int16)
        second = edges[second_coord][second_edge].astype(np.int16)
        delta = np.abs(first - second)
        pair_max = int(delta.max())
        pair_sum = int(delta.sum())
        pair_channels = int(np.count_nonzero(delta))
        pair_pixels = int(np.count_nonzero(np.any(delta != 0, axis=2)))
        comparisons += 1
        differing_pixels += pair_pixels
        differing_channels += pair_channels
        absolute_difference_sum += pair_sum
        maximum_difference = max(maximum_difference, pair_max)
        if pair_max:
            errors.append(
                {
                    "first": list(first_coord),
                    "second": list(second_coord),
                    "direction": direction,
                    "meanAbsoluteRgbDifference": pair_sum / delta.size,
                    "maximumChannelDifference": pair_max,
                    "differingPixels": pair_pixels,
                    "differingChannels": pair_channels,
                }
            )

    for y in range(GRID_SIZE):
        for x in range(GRID_SIZE):
            if x + 1 < GRID_SIZE:
                compare(
                    (x, y), (x + 1, y), "east",
                    1,
                    0,
                )
            if y + 1 < GRID_SIZE:
                compare(
                    (x, y), (x, y + 1), "south",
                    3,
                    2,
                )

    channels_compared = comparisons * SOURCE_SIZE * OVERLAP * 3
    audit: dict[str, object] = {
        "schemaVersion": 1,
        "sourceTiles": len(images),
        "adjacentPairs": comparisons,
        "overlapPixels": OVERLAP,
        "channelsCompared": channels_compared,
        "meanAbsoluteRgbDifference": (
            absolute_difference_sum / channels_compared if channels_compared else 0
        ),
        "maximumChannelDifference": maximum_difference,
        "differingPixels": differing_pixels,
        "differingChannels": differing_channels,
        "errorCount": len(errors),
        "errors": errors,
    }
    write_json_atomic(audit_path, audit)
    return audit


def compose_mosaic(
    images: dict[tuple[int, int], Path],
    *,
    grid_size: int = GRID_SIZE,
    source_size: int = SOURCE_SIZE,
    border: int = BORDER,
) -> Image.Image:
    cell_size = source_size - 2 * border
    if cell_size <= 0:
        raise BuildError("crop border leaves no source pixels")
    mosaic = Image.new("RGB", (grid_size * cell_size, grid_size * cell_size))
    for y in range(grid_size):
        for x in range(grid_size):
            path = images.get((x, y))
            if path is None:
                mosaic.close()
                raise BuildError(f"missing decoded tile {(x, y)}")
            with Image.open(path) as source:
                rgb = source.convert("RGB")
                if rgb.size != (source_size, source_size):
                    mosaic.close()
                    raise BuildError(
                        f"{path.name} is {rgb.size}, expected {(source_size, source_size)}"
                    )
                crop = rgb.crop((border, border, source_size - border, source_size - border))
                mosaic.paste(crop, (x * cell_size, y * cell_size))
                crop.close()
                rgb.close()
    return mosaic


def zoom_dimensions(
    width: int = MOSAIC_SIZE,
    height: int = MOSAIC_SIZE,
    *,
    min_zoom: int = MIN_ZOOM,
    max_zoom: int = MAX_ZOOM,
    tile_size: int = TILE_SIZE,
) -> list[dict[str, int]]:
    levels: list[dict[str, int]] = []
    for zoom in range(min_zoom, max_zoom + 1):
        divisor = 1 << (max_zoom - zoom)
        display_width = math.ceil(width / divisor)
        display_height = math.ceil(height / divisor)
        levels.append(
            {
                "zoom": zoom,
                "width": display_width,
                "height": display_height,
                "columns": math.ceil(display_width / tile_size),
                "rows": math.ceil(display_height / tile_size),
            }
        )
    return levels


def padded_tile(image: Image.Image, left: int, top: int, tile_size: int = TILE_SIZE) -> Image.Image:
    right = min(left + tile_size, image.width)
    bottom = min(top + tile_size, image.height)
    crop = image.crop((left, top, right, bottom)).convert("RGBA")
    if crop.size == (tile_size, tile_size):
        return crop
    padded = Image.new("RGBA", (tile_size, tile_size), (0, 0, 0, 0))
    padded.paste(crop, (0, 0))
    crop.close()
    return padded


def write_tile_pyramid(
    mosaic: Image.Image,
    root: Path,
    *,
    quality: int,
    workers: int,
) -> tuple[list[dict[str, int]], int]:
    levels = zoom_dimensions(mosaic.width, mosaic.height)
    total_bytes = 0

    def save_tile(tile: Image.Image, destination: Path) -> int:
        try:
            destination.parent.mkdir(parents=True, exist_ok=True)
            temp_path = destination.with_name(destination.stem + ".tmp.webp")
            tile.save(temp_path, "WEBP", quality=quality, method=4, exact=True)
            size = temp_path.stat().st_size
            _atomic_replace(temp_path, destination)
            return size
        finally:
            tile.close()

    for level in levels:
        zoom = level["zoom"]
        size = (level["width"], level["height"])
        scaled = mosaic if size == mosaic.size else mosaic.resize(size, Image.Resampling.LANCZOS)
        pending: set[concurrent.futures.Future[int]] = set()
        with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as executor:
            for y in range(level["rows"]):
                for x in range(level["columns"]):
                    tile = padded_tile(scaled, x * TILE_SIZE, y * TILE_SIZE)
                    destination = root / str(zoom) / str(x) / f"{y}.webp"
                    pending.add(executor.submit(save_tile, tile, destination))
                    if len(pending) >= workers * 2:
                        done, pending = concurrent.futures.wait(
                            pending, return_when=concurrent.futures.FIRST_COMPLETED
                        )
                        for future in done:
                            total_bytes += future.result()
            for future in concurrent.futures.as_completed(pending):
                total_bytes += future.result()
        if scaled is not mosaic:
            scaled.close()
        print(
            f"zoom {zoom}: {level['width']}x{level['height']}, "
            f"{level['columns']}x{level['rows']} tiles",
            flush=True,
        )
    return levels, total_bytes


def write_native_crops(mosaic: Image.Image, output_dir: Path) -> list[dict[str, object]]:
    output_dir.mkdir(parents=True, exist_ok=True)
    crop_size = 768
    anchors = {
        "northwest": (0, 0),
        "northeast": (mosaic.width - crop_size, 0),
        "center": ((mosaic.width - crop_size) // 2, (mosaic.height - crop_size) // 2),
        "southwest": (0, mosaic.height - crop_size),
        "southeast": (mosaic.width - crop_size, mosaic.height - crop_size),
    }
    records: list[dict[str, object]] = []
    for name, (left, top) in anchors.items():
        destination = output_dir / f"native-{name}.png"
        temp_path = destination.with_name(destination.stem + ".tmp.png")
        crop = mosaic.crop((left, top, left + crop_size, top + crop_size))
        crop.save(temp_path, "PNG", optimize=False, compress_level=6)
        crop.close()
        _atomic_replace(temp_path, destination)
        records.append(
            {
                "name": name,
                "path": f"qa/{destination.name}",
                "pixelBounds": [left, top, left + crop_size, top + crop_size],
                "worldBounds": [left, MOSAIC_SIZE - (top + crop_size), left + crop_size, MOSAIC_SIZE - top],
                "bytes": destination.stat().st_size,
            }
        )
    return records


def hash_file(path: Path, algorithm: str = "sha256") -> str:
    digest = hashlib.new(algorithm)
    with path.open("rb") as stream:
        while chunk := stream.read(8 * 1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def file_identity(path: Path) -> tuple[int, int, int]:
    stat = path.stat()
    return stat.st_size, stat.st_mtime_ns, stat.st_ino


def read_steam_build_id(pbo_path: Path) -> str | None:
    # .../steamapps/common/DayZ/Addons/archive.pbo -> .../steamapps/appmanifest_221100.acf
    try:
        steamapps = pbo_path.parents[3]
    except IndexError:
        return None
    manifest = steamapps / "appmanifest_221100.acf"
    if not manifest.is_file():
        return None
    text = manifest.read_text(encoding="utf-8", errors="replace")
    match = re.search(r'^\s*"buildid"\s+"([0-9]+)"\s*$', text, re.MULTILINE)
    return match.group(1) if match else None


def write_json_atomic(path: Path, value: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp_path = path.with_name(path.name + ".tmp")
    temp_path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")
    _atomic_replace(temp_path, path)


def write_manifest_atomic(path: Path, manifest: dict[str, object], artifact_bytes: int) -> None:
    output_bytes = manifest["outputBytes"]
    if not isinstance(output_bytes, dict):
        raise BuildError("manifest outputBytes must be an object")
    payload = b""
    for _ in range(4):
        payload = (json.dumps(manifest, indent=2) + "\n").encode("utf-8")
        exact_total = artifact_bytes + len(payload)
        if output_bytes["totalDirectory"] == exact_total:
            break
        output_bytes["totalDirectory"] = exact_total
    else:
        raise BuildError("manifest output byte count did not converge")
    temp_path = path.with_name(path.name + ".tmp")
    temp_path.write_bytes(payload)
    _atomic_replace(temp_path, path)


def directory_bytes(path: Path) -> int:
    return sum(
        item.stat().st_size
        for item in path.rglob("*")
        if item.is_file() and item.name != ".build.lock"
    )


def acquire_build_lock(output: Path) -> Path:
    lock_path = output / ".build.lock"
    try:
        descriptor = os.open(lock_path, os.O_CREAT | os.O_EXCL | os.O_WRONLY)
    except FileExistsError as exc:
        raise BuildError(
            f"output is locked by another or interrupted build: {lock_path}; "
            "inspect the process recorded there before removing that one file"
        ) from exc
    try:
        os.write(descriptor, f"pid={os.getpid()}\n".encode("ascii"))
    finally:
        os.close(descriptor)
    atexit.register(release_build_lock, lock_path)
    return lock_path


def release_build_lock(lock_path: Path) -> None:
    """Remove this process's lock, and only this process's: after an explicit
    release another build may hold a lock at the same path, and the exit hook
    of the first must not delete it."""
    try:
        owner = lock_path.read_text(encoding="ascii")
    except FileNotFoundError:
        return
    if owner.strip() != f"pid={os.getpid()}":
        return
    try:
        lock_path.unlink()
    except FileNotFoundError:
        pass


def parse_args(argv: Iterable[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pbo", type=Path, default=DEFAULT_PBO)
    parser.add_argument("--tool", type=Path, default=DEFAULT_TOOL)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument("--workers", type=int, default=min(4, os.cpu_count() or 1))
    parser.add_argument("--webp-quality", type=int, default=90)
    return parser.parse_args(argv)


def main(argv: Iterable[str] | None = None) -> int:
    args = parse_args(argv)
    started = time.perf_counter()
    timings: dict[str, float] = {}

    pbo_path = args.pbo.resolve()
    tool_path = args.tool.resolve()
    output = args.output.resolve()
    if not pbo_path.is_file():
        raise BuildError(f"PBO not found: {pbo_path}")
    if not tool_path.is_file():
        raise BuildError(f"ImageToPAA not found: {tool_path}")
    if not 1 <= args.webp_quality <= 100:
        raise BuildError("WebP quality must be between 1 and 100")
    output.mkdir(parents=True, exist_ok=True)
    # Completion is checked under the lock: a build that finishes between an
    # unlocked check and the acquisition would otherwise be overwritten.
    lock_path = acquire_build_lock(output)
    if (output / "manifest.json").exists():
        release_build_lock(lock_path)
        raise BuildError(
            f"output already contains a completed manifest: {output / 'manifest.json'}; "
            "use a new --output path for a new immutable build"
        )

    archive_identity = file_identity(pbo_path)
    phase = time.perf_counter()
    index = read_pbo_index(pbo_path)
    validate_source_identity(index)
    selected = select_satellite_entries(index)
    pbo_sha256 = hash_file(pbo_path)
    if file_identity(pbo_path) != archive_identity:
        raise BuildError("PBO changed while its directory and hashes were being validated")
    timings["indexAndHashSeconds"] = time.perf_counter() - phase
    print(f"validated PBO and selected {len(selected)} exact satellite members", flush=True)

    phase = time.perf_counter()
    sources, member_hashes = extract_selected(pbo_path, selected, output / "source-paa")
    if file_identity(pbo_path) != archive_identity:
        raise BuildError("PBO changed while selected members were being extracted")
    timings["extractSeconds"] = time.perf_counter() - phase

    phase = time.perf_counter()
    decoded = decode_all(tool_path, sources, output / "decoded-png", args.workers)
    timings["decodeSeconds"] = time.perf_counter() - phase

    phase = time.perf_counter()
    seam_audit = audit_seams(decoded, output / "seam-audit.json")
    timings["seamAuditSeconds"] = time.perf_counter() - phase
    if seam_audit["errorCount"]:
        raise BuildError(
            f"{seam_audit['errorCount']} adjacent source pairs have RGB overlap errors; "
            f"details are in {output / 'seam-audit.json'}"
        )
    print(f"verified all {seam_audit['adjacentPairs']} adjacent RGB overlaps", flush=True)

    phase = time.perf_counter()
    mosaic = compose_mosaic(decoded)
    master_path = output / "chernarusplus-satellite-master.png"
    master_temp = master_path.with_name(master_path.stem + ".tmp.png")
    mosaic.save(master_temp, "PNG", optimize=False, compress_level=6)
    _atomic_replace(master_temp, master_path)
    timings["mosaicSeconds"] = time.perf_counter() - phase

    phase = time.perf_counter()
    overview_path = output / "overview.png"
    overview_temp = overview_path.with_name("overview.tmp.png")
    overview = mosaic.resize((1920, 1920), Image.Resampling.LANCZOS)
    overview.save(overview_temp, "PNG", optimize=False, compress_level=6)
    overview.close()
    _atomic_replace(overview_temp, overview_path)
    native_crops = write_native_crops(mosaic, output / "qa")
    timings["qaImagesSeconds"] = time.perf_counter() - phase

    phase = time.perf_counter()
    levels, tile_bytes = write_tile_pyramid(
        mosaic,
        output / "tiles",
        quality=args.webp_quality,
        workers=args.workers,
    )
    mosaic_size = list(mosaic.size)
    timings["tilePyramidSeconds"] = time.perf_counter() - phase
    mosaic.close()

    timings["totalSeconds"] = time.perf_counter() - started
    master_bytes = master_path.stat().st_size
    overview_bytes = overview_path.stat().st_size
    source_bytes = sum(path.stat().st_size for path in sources.values())
    decoded_bytes = sum(path.stat().st_size for path in decoded.values())
    registration_applies = pbo_sha256 == REGISTRATION_AUDITED_PBO_SHA256
    manifest = {
        "schemaVersion": 1,
        "world": "chernarusplus",
        "bounds": {"xmin": 0, "xmax": 15360, "zmin": 0, "zmax": 15360},
        "raster": {"width": 15360, "height": 15360, "metresPerPixel": 1},
        "tiles": {
            "size": 256,
            "minZoom": 0,
            "maxZoom": 6,
            "urlTemplate": "tiles/{z}/{x}/{y}.webp",
            "format": "webp",
            "scheme": "xyz",
        },
        "source": {
            "archive": pbo_path.name,
            "archiveBytes": pbo_path.stat().st_size,
            "archiveSha256": pbo_sha256,
            "payloadSha1": index.trailer_sha1,
            "prefix": index.extensions.get("prefix"),
            "assetVersion": index.extensions.get("version"),
            "steamBuildId": read_steam_build_id(pbo_path),
            "memberPattern": "layers/S_XXX_YYY_lco.paa",
            "memberCount": len(selected),
            "memberSha256": member_hashes,
            "decoder": {
                "name": "ImageToPAA.exe",
                "sha256": hash_file(tool_path),
            },
        },
        "build": {
            "builtAt": datetime.now(timezone.utc).isoformat(),
            "script": "spikes/chernarus-satellite/build.py",
            "workers": args.workers,
            "webpQuality": args.webp_quality,
            "webpMethod": 4,
            "overviewSize": [1920, 1920],
        },
        "transform": {
            "filenameX": "east",
            "filenameY": "south",
            "rasterOrigin": "northwest",
            "tileRowOrigin": "north",
            "worldToRaster": {
                "u": "x",
                "v": "15360-z",
                "endpointPolicy": "world bounds are pixel-edge bounds; clamp markers to [0,width) and [0,height)",
            },
            "nativeZoom": 6,
            "nativeMetresPerPixel": 1,
            "logicalNativeSquare": 16384,
            "rasterIsStretched": False,
            "calibrationEvidence": {
                "kind": "asset-derived-stage0-rvmat",
                "sampleIndices": [[0, 0], [0, 31], [10, 10], [16, 16], [31, 0], [31, 31]],
                "textureScale": 0.001953125,
                "auditedArchiveSha256": REGISTRATION_AUDITED_PBO_SHA256,
                "appliesToCurrentArchive": registration_applies,
                "cropWorldBoundsVerified": (
                    [0, 0, 15360, 15360] if registration_applies else None
                ),
                "auditArtifact": ".scratch/chernarus-registration-audit/material-registration.json",
            },
        },
        "crop": {
            "sourceSize": [512, 512],
            "sharedOverlap": 32,
            "border": {"left": 16, "top": 16, "right": 16, "bottom": 16},
            "box": [16, 16, 496, 496],
            "stride": 480,
        },
        "zoomLevels": levels,
        "timings": {key: round(value, 3) for key, value in timings.items()},
        "outputBytes": {
            "sourcePaa": source_bytes,
            "decodedPng": decoded_bytes,
            "losslessMaster": master_bytes,
            "overview": overview_bytes,
            "webpTiles": tile_bytes,
            "totalDirectory": 0,
        },
        "qa": {"nativeCrops": native_crops, "seamAudit": "seam-audit.json"},
        "validation": {
            "measured": {
                "pboDirectoryAndTrailer": True,
                "gridComplete": True,
                "decodedTopMipSize": [512, 512],
                "assetMaterialRegistration": registration_applies,
                "adjacentRgbPairs": seam_audit["adjacentPairs"],
                "adjacentRgbErrors": seam_audit["errorCount"],
                "mosaicSize": mosaic_size,
                "tileCount": sum(level["columns"] * level["rows"] for level in levels),
                "edgeTilesTransparentPadded": True,
            },
            "unverified": [
                "absolute registration against surveyed in-game landmarks",
                "live in-game survey of outer-edge alignment",
                "live player marker direction, scale, and click targeting",
                "visual acceptance of WebP quality 90",
                "asset redistribution permission",
            ],
        },
    }
    write_manifest_atomic(output / "manifest.json", manifest, directory_bytes(output))
    release_build_lock(lock_path)
    print(
        f"complete in {timings['totalSeconds']:.1f}s; "
        f"output currently uses {directory_bytes(output) / (1024**3):.2f} GiB",
        flush=True,
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except BuildError as exc:
        print(f"error: {exc}", file=sys.stderr)
        raise SystemExit(1)
