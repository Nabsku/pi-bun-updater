# pi-bun

A small, dependency-free Go updater for the **official compiled Bun build** of [Pi](https://pi.dev/).

It installs alongside the npm/pnpm Pi package; it never replaces `pi`. The active compiled binary is exposed as `pi-bun`.

## What it does

- Resolves the latest GitHub release (or a requested `--version`).
- Selects the official asset for the running platform:
  - macOS: `arm64`, `amd64`
  - Linux: `arm64`, `amd64`
- Downloads the release archive and its upstream `SHA256SUMS` file.
- Verifies SHA-256 before extracting.
- Rejects archive paths, symlinks, and hardlinks that could escape the staging directory.
- Stores immutable releases in `~/.local/share/pi-bun/versions/<tag>/`.
- Atomically repoints `~/.local/bin/pi-bun` only after extraction succeeds.
- Refuses to overwrite a non-symlink named `pi-bun`.

There is intentionally no silent background updating. Run `pi-bun` when you want the current release, or schedule that command yourself after choosing your update policy.

## Install

```bash
git clone https://github.com/Nabsku/pi-bun-updater.git
cd pi-bun-updater
go build -o ~/.local/bin/pi-bun-update .
pi-bun-update --check
pi-bun-update
pi-bun --version
```

Ensure `~/.local/bin` is on `PATH`. Keep the updater as `pi-bun-update`; it maintains the separate `pi-bun` symlink to the installed Pi binary.

Subsequent updates:

```bash
pi-bun-update              # latest compatible official release
pi-bun-update --version v0.80.5
pi-bun-update --dry-run
```

## Build every supported target

```bash
make test
make release
```

This writes four statically-linked Go updater binaries to `dist/`: Darwin/Linux × arm64/amd64. Each binary chooses its matching Pi release asset at runtime.

## Options

```text
-repo owner/repository  GitHub repository (default: earendil-works/pi)
-version TAG            install a specific release tag
-root PATH              release store (default: ~/.local/share/pi-bun)
-bin-dir PATH           activation directory (default: ~/.local/bin)
-check                  resolve and print the latest compatible release
-dry-run                print paths without downloading or changing files
```

`--os` and `--arch` are intended for CI inspection/dry-runs. An actual installation must match the updater's own OS and architecture; activating a foreign executable is refused.
