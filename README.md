# pi-bun

A small, dependency-free Go updater for the **official compiled Bun build** of [Pi](https://pi.dev/).

By default it installs alongside the npm/pnpm Pi package and exposes the compiled binary as `pi-bun`; the updater remains `pi-bun-update`. Explicit `--force` mode instead activates the compiled binary as `pi` in the selected bin directory.

## Safety model

- Resolves the latest GitHub release, or a requested `--version`.
- Selects the official asset for the running platform: macOS/Linux × arm64/amd64.
- Downloads the release archive and upstream `SHA256SUMS`; verifies SHA-256 before extraction.
- Rejects archive traversal, symlinks, and hardlinks.
- Stores immutable releases in `~/.local/share/pi-bun/versions/<tag>/`.
- Safely repoints `~/.local/bin/pi-bun` by default, or `~/.local/bin/pi` with explicit `--force`, only after extraction succeeds.
- Refuses to overwrite a non-symlink activation target; `--force` may replace an existing `pi` symlink but never a regular executable.
- Takes a non-blocking per-store lock for `update`, `use`, and `prune`, preventing concurrent activation races.

There is intentionally no silent background updating. Run `pi-bun-update` when you choose to update, or use `status` / `update --check` from an explicit scheduler policy.

## Install

```bash
git clone https://github.com/Nabsku/pi-bun-updater.git
cd pi-bun-updater
go build -o ~/.local/bin/pi-bun-update .

pi-bun-update status
pi-bun-update update
pi-bun --version
```

Ensure `~/.local/bin` is on `PATH`.

## Operator commands

```bash
# Current active release vs latest compatible upstream release.
# Exit: 0 = current, 2 = update available, 1 = error.
pi-bun-update status
pi-bun-update status --json
pi-bun-update update --check  # status alias for scripts

# Install current latest or an explicit upstream version.
pi-bun-update update
pi-bun-update update --version v0.80.5
pi-bun-update update --dry-run

# Switch instantly to a previously installed release; no download.
pi-bun-update use v0.80.4

# Keep the active release plus the N newest installed versions.
pi-bun-update prune --keep 3
pi-bun-update prune --keep 3 --dry-run
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

All structured reports support `--json`. A `status` report includes `activation_name`, `active_version`, `latest_version`, `installed_versions`, `up_to_date`, and `update_available`.

## Releases

Pushing a SemVer tag creates a GitHub Release with four statically linked updater archives and a `checksums.txt` manifest through GoReleaser:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The release workflow has only `contents: write` permission and runs from the tagged source. It does not publish to a package registry.

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
-dry-run                preview update/use/prune mutations
-json                   emit machine-readable reports
-force                  activate as pi instead of pi-bun
-keep N                 retained newest versions for prune (default: 3)
```

`--os` and `--arch` are intended for CI inspection/dry-runs. An actual installation must match the updater's own OS and architecture; activating a foreign executable is refused.
