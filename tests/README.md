# Reliability checks

Run the local regressions from the repository root:

```sh
python3 tests/startup.py
python3 tests/release.py
node tests/backup.js
```

The Go suite requires `sqlite3` on PATH. Fixtures are copies of the shipped
C-Gate database, with test descriptions changed in temporary files. No unit
test accesses production projects. With the ignored module already initialized:

```sh
cd cgate-server/web
go vet ./...
go test -race ./...
```

The image synthesizes its module and pins the Go dependency. Every runtime
image depends on `web-test`, which runs vet and tests with the same Go and
SQLite versions used by the build. CI additionally runs race checks.

```sh
docker build --target web-test ./cgate-server
docker build -t cgate-check ./cgate-server
python3 tests/integration.py cgate-check
```

The integration script creates a unique disposable container and temporary
projects. It uses `--network none`, publishes no ports, maps no devices, and
removes the container/data on completion or failure. It tests welcome/response
alignment, replacement of a started project in a multiline list, persistence
of the replacement after a later save, invalid uploads, wrapped archives,
backup, bridge-only crash recovery, project selection, arguments and migration.
Run the same script against native ARM64 and AMD64 images where available;
Docker emulation is also supported.

The storage unit suite checks rollback after a rejected load, interruptions at
rename/commit boundaries, idempotent recovery, cancellation while waiting for
replacement, refusal to restore files while a project remains open, and stale
backup reporting. These are process-failure simulations, not certification of
filesystem behavior under physical power loss. External Toolkit clients do
not participate in the bridge operation lock; avoid concurrent project edits
while replacing projects or copying bitmap companions.

Release preflight is read-only and fails closed on registry errors. Releases
must have a matching stable `vX.Y.Z` tag and be newer than all existing numeric
image tags. Manual publication is disabled. To retry a partially published
release, use a new version; version tags are not overwritten by the workflow.
Pinned APK versions may eventually leave Alpine mirrors; update and retest
the pins together when that happens. Keep the Go versions in the Dockerfile
and `checks.yaml` synchronized.
