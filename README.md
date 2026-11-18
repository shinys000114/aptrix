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
$ aptrix add tools main --dist all https://example.com/tool_1.0_all.deb --checksums https://example.com/checksums.txt
$ aptrix ls boards rk3588 --dist noble --name 'linux-image-*'
$ aptrix find 'linux-image-*rk3588*' --newest            # search every publish
$ aptrix promote internal/rk3588 boards/rk3588 'linux-*' # copy newest versions, no re-upload
$ aptrix rm boards rk3588 '*-dbg'   # snapshots the affected repos first
$ aptrix prune-versions boards --keep 3
$ aptrix snapshot diff boards pre-rm-20261118-170000.123 # what changed since then
$ aptrix rollback boards pre-rm-20261118-170000.123      # restore and republish
$ aptrix snapshot prune boards --older-than 30d && aptrix gc
$ aptrix sync --prune               # also remove what the config no longer has
```

Package names are shell-style globs (`*`, `?`, `[...]`); quote them so the
shell does not expand them. Version conditions use dpkg ordering:
`--version '>=1.2'`, `'>=1.0,<2.0'`, `'!=1.1'`. Numbers compare by value, so
write full versions (`<9:6.6.144-202609010000`), not prefixes. `ls`, `find`,
`status`, `plan`, `snapshot list` and `snapshot diff` accept `--json`.

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

Optional per publish:

- `keep = 3` keeps the newest 3 versions of each package (per dist, component
  and architecture) after every `add` and `promote`; `add` then snapshots the
  target repos first, so `rollback` can undo it.
- `snapshot_max_age = "30d"` expires aptrix's automatic snapshots
  (`pre-add-`, `pre-rm-`, `pre-promote-`, `pre-prune-`, `pre-rollback-`) of
  that publish after each change. Manual `snapshot create` labels are kept.
  Freed packages stay in aptly's pool until `aptrix gc` (aptly
  `db cleanup`, server-wide).

### Retiring a publish

`sync --prune` only manages publishes listed in the config, so removing a
table does not remove the publish. To remove one (e.g. after merging it into
another with `promote`):

1. Empty it: `aptrix rm <alias> <component> '*'` (or `promote --move` out).
2. Drop its snapshots while it is still in the config:
   `aptrix snapshot prune <alias> --older-than 0s --include-manual`.
3. Add `retire = true` to its table. `add`/`promote` into it are refused, and
   `plan --prune` lists its published dists and repos for removal (non-empty
   ones need `--force`; repos another publish uses are kept).
4. `aptrix sync --prune`, then delete the table and run `aptrix gc`.

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
- `rm` snapshots every repo it touches (label `pre-rm-<time>`) before
  removing; `aptrix rollback <alias> <label>` restores those repos exactly
  (re-adds and removes packages) and republishes. A rollback snapshots the
  current state first (`pre-rollback-<time>`), so it can be undone too.
  Snapshots keep their packages in aptly's pool; drop old labels with
  `aptrix snapshot drop <alias> <label>`.
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
