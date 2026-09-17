#!/usr/bin/env bash
# Builds the hub's release archives into dist/: one static binary per
# platform with LICENSE and README beside it, the systemd unit and its
# environment example in the Linux archives, and SHA256SUMS over all of it.
#
#   scripts/release-hub.sh v0.1.0
#
# The binaries are reproducible for a given Go toolchain (-trimpath, no cgo,
# the version linked in) and so are the archives, written by scripts/archive
# rather than the host's tar or zip: entries sorted, owned by nobody, dated
# by the commit in UTC (SOURCE_DATE_EPOCH overrides), the binary alone
# executable. The release workflow runs this on a hub-v* tag; run it at the
# tagged commit with the same Go version, on any host, to check a published
# archive against the repository.
set -euo pipefail

version="${1:-}"
# SemVer 2.0.0 without build metadata (a "+" cannot appear in a container
# tag), with the prerelease identifiers checked one by one so a numeric one
# cannot carry a leading zero. The same expression guards the release
# workflow and the plugin's version parser; change all three together.
if ! printf '%s' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$'; then
  echo "usage: scripts/release-hub.sh v<major>.<minor>.<patch>[-<prerelease>]" >&2
  exit 2
fi
root="$(cd "$(dirname "$0")/.." && pwd)"
dist="$root/dist"
number="${version#v}"
epoch="${SOURCE_DATE_EPOCH:-$(git -C "$root" log -1 --format=%ct)}"

rm -rf "$dist"
mkdir -p "$dist"
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  os="${target%/*}"
  arch="${target#*/}"
  name="vyshka-hub_${number}_${os}_${arch}"
  stage="$dist/$name"
  mkdir -p "$stage"
  bin="vyshka-hub"
  if [ "$os" = windows ]; then
    bin="vyshka-hub.exe"
  fi
  (cd "$root" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags="-s -w -X github.com/That1Drifter/vyshka/hub.Version=$version" \
    -o "$stage/$bin" ./hub/cmd/vyshka-hub)
  cp "$root/LICENSE" "$root/README.md" "$stage/"
  if [ "$os" = linux ]; then
    cp "$root/deploy/vyshka-hub.service" "$root/deploy/hub.env.example" "$stage/"
  fi
  suffix="tar.gz"
  if [ "$os" = windows ]; then
    suffix="zip"
  fi
  (cd "$root" && go run ./scripts/archive -out "$dist/$name.$suffix" -epoch "$epoch" -exec "$bin" "$stage")
  rm -rf "$stage"
done
(cd "$dist" && sha256sum -- * > SHA256SUMS)
echo "release-hub: built $version with $(go version | awk '{print $3}') into $dist" >&2
cat "$dist/SHA256SUMS"
