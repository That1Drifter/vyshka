import hashlib
import importlib.util
import struct
import sys
import tempfile
import unittest
from pathlib import Path

from PIL import Image


MODULE_PATH = Path(__file__).with_name("build.py")
SPEC = importlib.util.spec_from_file_location("chernarus_satellite_build", MODULE_PATH)
build = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
sys.modules[SPEC.name] = build
SPEC.loader.exec_module(build)


def entry(name: str, method: int, original: int, reserved: int, timestamp: int, size: int) -> bytes:
    return name.encode("utf-8") + b"\x00" + struct.pack(
        "<IIIII", method, original, reserved, timestamp, size
    )


def pbo_bytes(files: list[tuple[str, bytes]]) -> bytes:
    raw = entry("", build.VERSION_METHOD, 0, 0, 0, 0)
    raw += b"prefix\x00DZ\\worlds\\chernarusplus\\data\x00\x00"
    for name, body in files:
        raw += entry(name, 0, len(body), 0, 123, len(body))
    raw += entry("", 0, 0, 0, 0, 0)
    raw += b"".join(body for _, body in files)
    return raw + b"\x00" + hashlib.sha1(raw).digest()


class PBOIndexTests(unittest.TestCase):
    def test_reads_offsets_and_exact_bodies(self) -> None:
        bodies = [("layers\\S_000_000_lco.paa", b"first"), ("other.bin", b"second")]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "sample.pbo"
            path.write_bytes(pbo_bytes(bodies))
            index = build.read_pbo_index(path)

            self.assertEqual(index.extensions["prefix"], r"DZ\worlds\chernarusplus\data")
            self.assertEqual([item.name for item in index.entries], [item[0] for item in bodies])
            with path.open("rb") as stream:
                for indexed, (_, expected) in zip(index.entries, bodies):
                    stream.seek(indexed.offset)
                    self.assertEqual(stream.read(indexed.data_size), expected)

    def test_rejects_bad_trailer_checksum(self) -> None:
        damaged = bytearray(pbo_bytes([("a", b"body")]))
        damaged[-1] ^= 0xFF
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "damaged.pbo"
            path.write_bytes(damaged)
            with self.assertRaisesRegex(build.BuildError, "SHA-1"):
                build.read_pbo_index(path)

    def test_selection_is_exact_and_requires_complete_grid(self) -> None:
        exact = build.PBOEntry(r"layers\S_000_000_lco.paa", 0, 1, 0, 0, 1, 10)
        lookalike = build.PBOEntry(r"other\S_000_000_lco.paa", 0, 1, 0, 0, 1, 11)
        index = build.PBOIndex({}, (exact, lookalike), 0, 0, "00" * 20)
        old_grid = build.GRID_SIZE
        try:
            build.GRID_SIZE = 1
            selected = build.select_satellite_entries(index)
        finally:
            build.GRID_SIZE = old_grid
        self.assertEqual(list(selected), [(0, 0)])

        incomplete = build.PBOIndex({}, (lookalike,), 0, 0, "00" * 20)
        try:
            build.GRID_SIZE = 1
            with self.assertRaisesRegex(build.BuildError, "not exactly"):
                build.select_satellite_entries(incomplete)
        finally:
            build.GRID_SIZE = old_grid

    def test_rejects_another_terrain_prefix(self) -> None:
        index = build.PBOIndex(
            {"product": "dayz", "prefix": r"DZ\worlds\enoch\data"},
            (),
            0,
            0,
            "00" * 20,
        )
        with self.assertRaisesRegex(build.BuildError, "unexpected PBO prefix"):
            build.validate_source_identity(index)


class RasterTests(unittest.TestCase):
    def test_decode_does_not_reuse_a_newer_png_by_timestamp(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "source.paa"
            source.write_bytes(b"changed source")
            destination_dir = root / "decoded"
            destination_dir.mkdir()
            old_destination = destination_dir / "source.png"
            Image.new("RGBA", (512, 512), (1, 2, 3, 255)).save(old_destination)
            calls = []
            original = build._decode_one

            def fake_decode(tool_path: Path, source_path: Path, destination: Path) -> None:
                calls.append((tool_path, source_path, destination))
                Image.new("RGBA", (512, 512), (4, 5, 6, 255)).save(destination)

            try:
                build._decode_one = fake_decode
                result = build.decode_all(
                    root / "tool.exe", {(0, 0): source}, destination_dir, workers=1
                )
            finally:
                build._decode_one = original

            self.assertEqual(len(calls), 1)
            with Image.open(result[(0, 0)]) as image:
                self.assertEqual(image.getpixel((0, 0)), (4, 5, 6, 255))

    def test_crop_places_x_east_and_y_south(self) -> None:
        colors = {
            (0, 0): (255, 0, 0),
            (1, 0): (0, 255, 0),
            (0, 1): (0, 0, 255),
            (1, 1): (255, 255, 0),
        }
        with tempfile.TemporaryDirectory() as directory:
            paths = {}
            for coordinate, color in colors.items():
                path = Path(directory) / f"{coordinate}.png"
                source = Image.new("RGB", (4, 4), (255, 0, 255))
                for y in range(1, 3):
                    for x in range(1, 3):
                        source.putpixel((x, y), color)
                source.save(path)
                source.close()
                paths[coordinate] = path
            mosaic = build.compose_mosaic(paths, grid_size=2, source_size=4, border=1)
            try:
                self.assertEqual(mosaic.size, (4, 4))
                self.assertEqual(mosaic.getpixel((0, 0)), colors[(0, 0)])
                self.assertEqual(mosaic.getpixel((3, 0)), colors[(1, 0)])
                self.assertEqual(mosaic.getpixel((0, 3)), colors[(0, 1)])
                self.assertEqual(mosaic.getpixel((3, 3)), colors[(1, 1)])
                for y in range(mosaic.height):
                    for x in range(mosaic.width):
                        self.assertNotEqual(mosaic.getpixel((x, y)), (255, 0, 255))
            finally:
                mosaic.close()

    def test_zoom_math_preserves_native_raster_without_stretching(self) -> None:
        levels = build.zoom_dimensions()
        self.assertEqual(
            [(level["width"], level["columns"]) for level in levels],
            [(240, 1), (480, 2), (960, 4), (1920, 8), (3840, 15), (7680, 30), (15360, 60)],
        )
        self.assertEqual(sum(level["columns"] * level["rows"] for level in levels), 4810)

    def test_last_tile_is_transparent_padded(self) -> None:
        image = Image.new("RGB", (300, 270), (10, 20, 30))
        tile = build.padded_tile(image, 256, 256)
        try:
            self.assertEqual(tile.size, (256, 256))
            self.assertEqual(tile.getpixel((43, 13)), (10, 20, 30, 255))
            self.assertEqual(tile.getpixel((44, 13)), (0, 0, 0, 0))
            self.assertEqual(tile.getpixel((43, 14)), (0, 0, 0, 0))
        finally:
            tile.close()
            image.close()


if __name__ == "__main__":
    unittest.main()
