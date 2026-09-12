"""Serve only the satellite prototype viewer and its generated data on loopback."""

import argparse
from functools import partial
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


class PrototypeHandler(SimpleHTTPRequestHandler):
    def __init__(self, *args, data_directory, **kwargs):
        self.data_directory = data_directory
        super().__init__(*args, **kwargs)

    def translate_path(self, path):
        original_directory = self.directory
        try:
            if path.split("?", 1)[0].startswith("/data/"):
                self.directory = self.data_directory
                path = path[len("/data"):]
            return super().translate_path(path)
        finally:
            self.directory = original_directory

    def list_directory(self, path):
        self.send_error(404, "Directory listing is disabled")
        return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data", type=Path, default=Path(".scratch/chernarus-satellite"))
    parser.add_argument("--port", type=int, default=8766)
    args = parser.parse_args()
    data = args.data.resolve()
    if (data / ".build.lock").exists():
        parser.error(f"Dataset build is active or interrupted: {data / '.build.lock'}")
    if not (data / "manifest.json").is_file():
        parser.error(f"Generate the dataset first: missing {data / 'manifest.json'}")
    handler = partial(
        PrototypeHandler,
        directory=str(Path(__file__).resolve().parent / "viewer"),
        data_directory=str(data),
    )
    server = ThreadingHTTPServer(("127.0.0.1", args.port), handler)
    print(f"Chernarus prototype: http://127.0.0.1:{args.port}/", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
