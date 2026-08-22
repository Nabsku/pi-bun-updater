# pi-bun

A small, standalone Go updater for the **official compiled Bun build** of [Pi](https://pi.dev/).

By default it installs alongside the npm/pnpm Pi package and exposes the compiled binary as `pi-bun`; the updater remains `pi-bun-update`. Explicit `--force` mode instead activates the compiled binary as `pi` in the selected bin directory.

## Safety model

- Resolves the latest GitHub release, or a requested `--version`.
- Selects the official asset for the running platform: macOS/Linux × arm64/amd64.
- Downloads the release archive and upstream `SHA256SUMS`; verifies SHA-256 before extraction.
- Rejects archive traversal, symlinks, and hardlinks.
- Stores each verified release by repository, OS, architecture, exact tag, and archive digest. A manifest records that provenance plus archive and extracted-binary hashes.
- Revalidates the manifest and binary hash before reusing or activating an installation. Legacy tag-only installs are never trusted or adopted by name.
- Atomically repoints `~/.local/bin/pi-bun` by default, or `~/.local/bin/pi` with explicit `--force`, only after extraction and manifest publication succeed.
- Refuses to overwrite a non-symlink activation target; `--force` may replace an existing `pi` symlink but never a regular executable.
- Takes a non-blocking per-store lock for mutations, preventing concurrent activation and cleanup races.
- Never replaces a valid newer activation with GitHub's older latest release. An explicit `--version` is required to authorize a downgrade, repair, or same-tag digest change.

Atomic replacement uses the native Linux/macOS rename-exchange primitive. If the selected filesystem does not support it, replacement fails without falling back to a remove-then-create sequence.

There is intentionally no silent background updating. Run `pi-bun-update` when you choose to update, or use `status` / `update --check` from an explicit scheduler policy.

## Install

### Homebrew

```bash
brew install Nabsku/tap/pi-bun-updater

pi-bun-update status
pi-bun-update update
pi-bun --version
```

### Build from source

```bash
git clone https://github.com/Nabsku/pi-bun-updater.git
cd pi-bun-updater
go build -o ~/.local/bin/pi-bun-update .
```

Ensure `~/.local/bin` is on `PATH` when building from source.

## Operator commands

```bash
# Current active release vs latest compatible upstream release.
# State: not_installed | current | behind | ahead | corrupt
# Exit: 0 = current/ahead, 2 = behind, 1 = not_installed/corrupt/error.
pi-bun-update status
pi-bun-update status --json
pi-bun-update update --check  # status alias for scripts

# Install current latest or an explicit upstream version.
pi-bun-update update
pi-bun-update update --version v0.80.5
pi-bun-update update --dry-run

# Switch instantly to a previously installed release; no download.
pi-bun-update use v0.80.4

# Keep active release identities plus the N newest installed version tags.
pi-bun-update prune --keep 3
pi-bun-update prune --keep 3 --dry-run

# Validate the local store and both managed activations without network access.
# Exit: 0 = healthy, 1 = one or more findings/error.
pi-bun-update doctor
pi-bun-update doctor --json

# Recover only recognized interrupted activation transactions.
pi-bun-update doctor --repair

# Explicitly remove recognized inactive data. Neither class is deleted implicitly.
pi-bun-update purge --legacy --dry-run
pi-bun-update purge --legacy
pi-bun-update purge --orphans
```

### Activate directly as `pi`

`--force` changes the managed activation name from `pi-bun` to `pi`:

```bash
pi-bun-update update --force
pi-bun-update status --force
pi-bun-update use --force v0.80.4
pi-bun-update prune --force --keep 3
pi --version
```

Use `--force` consistently for `update`, `status`, and `use`; those commands operate on the selected activation symlink. `prune` always protects versions referenced by both managed names (`pi-bun` and `pi`), regardless of the flag. This creates `pi` only in `--bin-dir` and does not uninstall the npm/pnpm Pi package. Your `PATH` order decides which `pi` command wins. A regular existing `pi` executable is never overwritten.

All structured reports support `--json`. A `status` report includes `status`, optional `reason`, `activation_name`, `active_version`, `latest_version`, `installed_versions`, `up_to_date`, and `update_available`. `update_available` is true only for `behind`.

The v2 store lives below `~/.local/share/pi-bun/versions/v2/`. Existing `versions/<tag>/` installs remain untouched, but `use` and `prune` ignore them. A normal update replaces a legacy activation with a freshly downloaded and verified v2 install when its claimed tag is not newer than latest; otherwise pass an explicit `--version` to state the intended target.

`doctor` is offline and read-only unless `--repair` is supplied. Repair is deliberately narrow: it invokes the same fail-closed recovery used before activation and does not delete corrupt installations. `purge` requires `--legacy`, `--orphans`, or both. It protects every activation target, removes only exact legacy layouts or private transaction directories, and preserves staging data unless a valid published replacement exists.

## Releases

Pushing a SemVer tag creates a GitHub Release with four statically linked updater archives and a `checksums.txt` manifest through GoReleaser:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The release workflow has only `contents: write` permission and runs from the tagged source. The public [`Nabsku/homebrew-tap`](https://github.com/Nabsku/homebrew-tap) independently checks for new releases and opens tested formula update pull requests; this repository holds no tap write credential.

## Build every supported target

```bash
make test
make release
```

This writes four statically-linked updater binaries to `dist/`: Darwin/Linux × arm64/amd64. Each binary selects its matching Pi release asset at runtime.

## Options

```text
-repo owner/repository  GitHub repository (default: earendil-works/pi)
-version TAG            install a specific release tag
-root PATH              release store (default: ~/.local/share/pi-bun)
-bin-dir PATH           activation directory (default: ~/.local/bin)
-check                  status-only alias for update
-dry-run                preview update/use/prune/purge mutations
-json                   emit machine-readable reports
-force                  activate as pi instead of pi-bun
-keep N                 retained newest versions for prune (default: 3)
-repair                 recover recognized activation transactions (doctor only)
-legacy                 remove recognized tag-only installs (purge only)
-orphans                remove recognized inactive workspaces (purge only)
```

`--os` and `--arch` are intended for CI inspection/dry-runs. An actual installation or `use` activation must match the updater's own OS and architecture; activating a foreign executable is refused.
