# Third-Party Notices

## Redis 5.0.14.1 for Windows

- Upstream project: https://github.com/redis/redis/tree/5.0
- Windows build: https://github.com/tporadowski/redis/releases/tag/v5.0.14.1
- Windows port maintainer: Tomasz Poradowski
- License: BSD-3-Clause (the full text is included at `tools/redis/LICENSE.txt`)

Bundled files under `tools/redis/`:

- `redis-server.exe`
- `redis-cli.exe`
- `EventLog.dll`
- `README.txt`
- `RELEASENOTES.txt`
- `00-RELEASENOTES`
- `LICENSE.txt`

The Redis binaries are redistributed unmodified. The gateway uses Redis only as a local quota store; no Redis source code is modified.

## models5 / models.dev / OpenRouter pricing data

The gateway fetches public model pricing catalogs at runtime based on `config.json`. Those data sources remain subject to their own terms and licenses; no catalog data is bundled in this repository.
