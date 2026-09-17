# Releasing

Three artifacts, versioned independently: the hub and the DayZ plugin are each released
from their own tag, and the protocol document carries a draft number in its header with
no tag. Nothing is published by hand.

| Artifact | Its version lives in | Tag | What the tag publishes |
|---|---|---|---|
| Hub (the binary, the panel it embeds, the container image) | The tag; linked into the binary (`vyshka-hub version`, `/healthz`) | `hub-v<major>.<minor>.<patch>` | A GitHub release with one archive per platform and `SHA256SUMS`; `ghcr.io/that1drifter/vyshka-hub:<version>` for linux/amd64 and linux/arm64, plus `:latest` unless the version is a prerelease |
| DayZ plugin | `PLUGIN_VERSION` in `plugins/dayz/mod/scripts/3_Game/Vyshka/VyshkaPlugin.c`, which the manifest reports and `mod.cpp` carries | `dayz-plugin-v<version>`, equal to `PLUGIN_VERSION` or the workflow refuses | A GitHub release with `vyshka-dayz-plugin_<version>.zip` (the `@Vyshka` folder), `Vyshka.pbo.sha256`, and `SHA256SUMS` |
| Protocol document | The header of `spec/protocol.md` (draft number and date until 1.0) | None | Nothing; the document is its own record, and a change lands with the implementation that needs it |

A version is `v<major>.<minor>.<patch>`, optionally with a prerelease suffix such as
`v0.2.0-rc.1`: the workflow marks that GitHub release as a prerelease and does not move the
image's `latest` tag. SemVer build metadata (`+build.1`) is refused, because a container
tag cannot carry a `+`.

## Before any tag

1. CI is green on `main` at the commit to tag. A tag can point anywhere, so the workflow
   runs the tests again, but a red `main` is not a release candidate.
2. `CHANGELOG.md`: under the artifact's section, rename `### [Unreleased]` to
   `### [<version>] - <date>` and add a fresh, empty `### [Unreleased]` above it. That
   commit is the one to tag.
3. `ROADMAP.md`, if the release moves an item between states, in the same commit.
4. Rehearse when the workflow or the tooling changed: run the **Release** workflow by hand
   (`gh workflow run release.yml -f artifact=hub -f version=v0.1.0 --ref <branch>`). A
   dispatch builds everything, grades it, uploads the result as a workflow artifact, and
   publishes nothing. Download the artifact and look inside.

## The hub

```
git tag -a hub-v0.1.0 -m "hub 0.1.0"
git push origin hub-v0.1.0
gh run watch
```

The workflow tests, builds the archives with `scripts/release-hub.sh`, unpacks the Linux
archive and runs the hub conformance suite against that binary, uploads the archives,
builds the image for both platforms, and only then creates the release and pushes the
image. Afterwards:

- `gh release view hub-v0.1.0` lists the assets. Download one with `SHA256SUMS` and run
  `sha256sum --ignore-missing -c SHA256SUMS` (the manifest lists every archive; without
  the flag the four you did not download count as failures).
- `docker run --rm ghcr.io/that1drifter/vyshka-hub:0.1.0 version` prints the tag, and
  `docker run --rm --platform linux/arm64 ghcr.io/that1drifter/vyshka-hub:0.1.0 version`
  does too on a host with emulation; the workflow already inspected both binaries' ELF
  headers before publishing.
- **The first image publish only**: a package published to a user namespace is private
  until made public. Open the package's settings on GitHub (Packages, `vyshka-hub`, Package
  settings), change the visibility to public, and confirm the package is linked to the
  repository so the README shows beside it. Later pushes keep the visibility.
- To check a download against the repository: at the tagged commit, with the Go version
  the release notes name, run `scripts/release-hub.sh <version>` and compare `SHA256SUMS`.
  The binaries match byte for byte from a checkout that honours `.gitattributes` (every
  text file LF, which is what the embedded panel files and the packed README need), and
  so do the archives on any host, because `scripts/archive` writes them rather than the
  host's tar or zip.

## The DayZ plugin

The slice that changes the mod bumps `PLUGIN_VERSION`; a release does not. The tag repeats
that number.

```
git tag -a dayz-plugin-v0.7.0 -m "DayZ plugin 0.7.0"
git push origin dayz-plugin-v0.7.0
gh run watch
```

The workflow checks the tag against `PLUGIN_VERSION` (read by `vyshka-dayz version`, the
same code that writes the number into `mod.cpp`), runs the packer's tests, builds the mod,
records the PBO digest, zips the `@Vyshka` folder, and creates the release. The PBO is
reproducible (`plugins/dayz/README.md`, "Building"): `go run ./plugins/dayz/cmd/vyshka-dayz
build` at the tag prints the same digest on any machine with a full clone. A shallow clone
stamps its boundary commit instead and the tool warns; the workflow checks out the full
history for that reason.

### No Steam Workshop item

The plugin is a server-side mod loaded with `-serverMod`; clients never load it and are
never asked for it, so nothing pairs against a Workshop id. The GitHub release zip is the
distribution, decided 2026-09-17 with the first tag. A Workshop item would add a Steam
account to the release path for no operator benefit.

## The protocol document

No tag. A substantive edit bumps the draft number and date in the header of
`spec/protocol.md` in the same commit (`CONTRIBUTING.md`), and the change is recorded in
the changelog under the artifact that carried it. Protocol 1.0 is a separate decision
(`ROADMAP.md`, Horizon 4) and will get its own release note when it happens.
