#!/usr/bin/env bash
# Builds the hub's release archives into dist/: one static binary per
# platform with LICENSE and README beside it, the systemd unit and its
# environment example in the Linux archives, and SHA256SUMS over all of it.
#
#   scripts/release-hub.sh v0.1.0
#
# The binaries are reproducible for a given Go toolchain (-trimpath, no cgo,
# the version linked in) and so are the archives: entries sorted, owned by
# nobody, dated by the commit (SOURCE_DATE_EPOCH overrides). The release
# workflow runs this on a hub-v* tag; run it at the tagged commit with the
# same Go version to check a published archive against the repository.
set -euo pipefail

version="${1:-}"
# SemVer without build metadata, the same rule as the release workflow: a
# "+" cannot appear in a container tag.
if ! printf '%s' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$'; then
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
  find "$stage" -exec touch -d "@$epoch" {} +
  if [ "$os" = windows ]; then
    if command -v zip >/dev/null 2>&1; then
      (cd "$dist" && zip -q -r -X "$name.zip" "$name")
    else
      # Git Bash on Windows ships no zip; 7-Zip writes the same archive.
      (cd "$dist" && 7z a -tzip -bd -y "$name.zip" "$name" >/dev/null)
    fi
  else
    tar -C "$dist" --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" \
      -cf - "$name" | gzip -n > "$dist/$name.tar.gz"
  fi
  rm -rf "$stage"
done
(cd "$dist" && sha256sum -- * > SHA256SUMS)
echo "release-hub: built $version with $(go version | awk '{print $3}') into $dist" >&2
cat "$dist/SHA256SUMS"
