# aptrix

Manage [aptly](https://www.aptly.info/) **dist × component matrices** from one TOML file.

aptly maps every (distribution, component) pair to its own local repo, so a
repository with 4 dists and 9 components is 36 repos and 4 publishes. Adding a
component, a dist, or a package to "all dists" means dozens of API calls.
aptrix lets you declare the matrix and does the bookkeeping:

```console
$ aptrix plan                       # what would change (read-only)
$ aptrix sync                       # create missing repos / components / dists
$ aptrix status boards              # package count per dist x component
$ aptrix add boards rk3588 *.deb    # dist taken from the "~<dist>" version suffix
$ aptrix add tools main tool_1.0_all.deb --dist all
$ aptrix ls boards rk3588 --dist noble
$ aptrix rm boards rk3588 linux-image-6.6-rk3588 --keep 3   # keep newest 3 versions
$ aptrix sync --prune               # also remove what the config no longer has
```

It talks to the aptly REST API (`aptly api serve`), so it works the same from a
workstation, a CI agent or the aptly host. Requires **aptly 1.6 or newer**
(publish `sources` API).

## Install

From a release (`.deb` for amd64/arm64, or a tarball):

```sh
sudo apt install ./aptrix_<version>_amd64.deb
```

From source (Go 1.24+):

```sh
go install github.com/shinys000114/aptrix/cmd/aptrix@latest
```

## Configuration

See [examples/aptrix.example.toml](examples/aptrix.example.toml). Lookup order:
`-c FILE`, `$APTRIX_CONFIG`, `./aptrix.toml`, `~/.config/aptrix/aptrix.toml`,
`/etc/aptrix/aptrix.toml`.

```toml
[aptly]
url = "http://127.0.0.1:8080"

[signing]
gpg_key = "..."
passphrase_file = "/path/on/aptly/server"

[publish.boards]
storage = "filesystem:public"
prefix = "boards"
dists = ["jammy", "noble"]
components = ["main", "rk3588"]
architectures = ["arm64"]

[publish.boards.only]          # optional: component only in some dists
rk3588 = ["noble"]
```

### One config for many hosts

`-c` and `APTRIX_CONFIG` also accept an `http(s)://` URL. Serve one
`aptrix.toml` from the aptly host and point CI agents and workstations at it,
so a dist or component change is made in one place:

```sh
export APTRIX_CONFIG=http://aptly.example.lan:8089/aptrix.toml
aptrix plan
```

The URL is fetched on every run (no cache) and any failure aborts. The file
contains no secrets, but it describes your repository layout, so serve it on
an internal network only. Hosts that reach aptly through a different address
can keep the shared file and set `APTRIX_URL`.

Environment: `APTRIX_URL` overrides `aptly.url`; `APTRIX_TOKEN` (Bearer) or
`APTRIX_USERNAME`/`APTRIX_PASSWORD` (basic auth) for an aptly API behind a
reverse proxy.

## Safety

- `plan`, `status` and `ls` use a client that refuses any non-GET request.
- `sync` shows the plan and asks for confirmation; without a terminal it
  requires `--yes`, so CI jobs never hang on a prompt. `rm` works the same way.
- Nothing is removed without `--prune`. Even with `--prune`, a component or
  dist whose repos still contain packages stays published, and its repo is
  kept, unless `--force` is also given (aptly itself would delete it).
- Only publishes whose storage/prefix appear in the config are touched.
- If another run left staged publish changes, `sync` stops; use
  `--discard-staged` after checking them.
- `add` fails when aptly reports a conflicting package (same
  name/version/arch, different content). `--force-replace` replaces it and
  overwrites the published file.
- `signing.skip` is only accepted against a loopback aptly (tests).

## Development

```sh
make test          # vet + unit tests
make integration   # builds aptly 1.6.3 into .cache/ and runs end-to-end tests on 127.0.0.1
make snapshot      # goreleaser dry run: binaries, tarballs, .deb in dist/
```

Releases: push a `vX.Y.Z` tag; GitHub Actions runs GoReleaser and attaches
binaries, tarballs and `.deb` packages to the release.

## License

GPL-3.0-only. See [LICENSE](LICENSE).
