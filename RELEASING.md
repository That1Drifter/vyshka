# Releasing

Three artifacts, versioned independently, each released from its own tag. Nothing is
published by hand except the Steam Workshop item, which needs a Steam account.

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
  text file LF, which is what the embedded panel files and the packed README need); the
  archives match when GNU tar and gzip are used.

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

### The Steam Workshop

The Workshop item is the same `@Vyshka` folder, uploaded with the DayZ Tools Publisher
(Steam, DayZ Tools, Publisher). It is a GUI signed in to a Steam account, so this step is
manual and done by whoever holds the account.

1. Check out the tag in a full clone and build: `go run ./plugins/dayz/cmd/vyshka-dayz
   build`. The printed digest must equal `Vyshka.pbo.sha256` on the GitHub release; if it
   does not, stop.
2. In Publisher choose the folder `plugins/dayz/build/@Vyshka`. First publish: a new item
   titled **Vyshka**, the overview from the plugin README's first paragraph, the change
   notes from the changelog entry, visibility public. Later versions: update the existing
   item with the same folder and the changelog entry as the change notes.
3. Publisher writes `meta.cpp` (with the item's `publishedid`) into the folder. It is
   under the gitignored `build/` and is never committed; do not edit it by hand.
4. After the first publish, record the item's id and URL in `plugins/dayz/README.md` under
   "Installing on a server", so operators can subscribe from the launcher or fetch it with
   `steamcmd +workshop_download_item 221100 <id>`. The item carries a server mod: clients
   never need it.

## The protocol document

No tag. A substantive edit bumps the draft number and date in the header of
`spec/protocol.md` in the same commit (`CONTRIBUTING.md`), and the change is recorded in
the changelog under the artifact that carried it. Protocol 1.0 is a separate decision
(`ROADMAP.md`, Horizon 4) and will get its own release note when it happens.
