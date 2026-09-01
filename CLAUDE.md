# Working on this repository

A Home Assistant add-on wrapping Schneider Electric's C-Gate Server, plus a
small Go web console (`cgate-server/web`) that bridges C-Gate's TCP interfaces
to HTTP/WebSocket and serves the ingress panel.

## Layout

- `cgate-server/config.yaml` — add-on manifest. **`version:` is the published
  image tag**, so it must be bumped for every release.
- `cgate-server/run.sh` — the entrypoint. Sets up `/data`, rewrites C-Gate's
  config, starts the Go bridge under a restart loop, then `exec`s Java as PID 1.
- `cgate-server/web/main.go` — the whole bridge. Single file, deliberately: the
  Dockerfile copies `web/main.go` and `web/console.html` by name, so **adding a
  `.go` file means updating that `COPY` line**.
- `cgate-server/cgate-dist/` — the unmodified C-Gate distribution. Treat as
  vendor code; `cgate-dist/help/cmds.txt` is the command reference and is worth
  grepping before guessing at C-Gate syntax (use `grep -a`; it is not clean
  UTF-8).

## Building and testing

There is no Go toolchain on the usual dev machine. Run the tests in the image's
own toolchain:

```sh
cd cgate-server && docker run --rm -v "$PWD/web":/src:ro -w /tmp/b golang:1.25-alpine sh -c '
cp /src/main.go /src/main_test.go /src/console.html . &&
go mod init cgate-web >/dev/null 2>&1 &&
go get golang.org/x/net/websocket >/dev/null 2>&1 &&
gofmt -l . && go vet ./... && go test ./...'
```

`go.mod`/`go.sum` are gitignored and synthesised at build time — do not commit
them. `docker build --target web-build ./cgate-server` checks the Go stage alone.

For anything touching C-Gate's behaviour, build and run the real thing rather
than reasoning about it — several assumptions in this repo's history did not
survive contact:

```sh
docker build -t cgate-test ./cgate-server
docker run -d --name cgate-test -v "$PWD/somedata":/data cgate-test   # needs /data/options.json
docker exec cgate-test sh -c 'printf "project dir\nproject list\n" | nc -w 6 127.0.0.1 20023'
```

## The bridge's connection model

- **Nothing dials C-Gate with `cmdSession.mu` held for longer than one attempt.**
  `connect()` makes a single bounded dial and returns an error; `drop()` closes
  the session and marks it down; `maintain()` is the one goroutine allowed to
  wait. The earlier version redialled from inside `send()` with the mutex held
  and no time limit, so every request — the ingress panel's included — queued
  behind that dial for the whole of a C-Gate restart. If you add a code path
  that reconnects, reconnect through `maintain()`, not under the lock.
- `maintain()` polls on `dialRetryInterval`, not on `commandHeartbeat`. Sleeping
  the heartbeat interval means a session that drops between heartbeats is not
  rebuilt until the next one, which on an idle console is not rebuilt at all.
- `/health` is liveness and stays 200 while the bridge is serving; `/ready` is
  readiness and answers 503 until all three connections are up. Do not make
  `/health` fail when C-Gate is down — Supervisor uses it to decide whether to
  restart, and C-Gate takes up to a minute to sync its networks on a cold start.
- The event and status streams carry **no read deadline**, deliberately. Both
  are legitimately silent on a quiet site; TCP keepalive is the backstop.
- `web/main.go` here and `C-Gate-Server-Container/web/main.go` are parallel
  implementations of the same bridge, not one shared file. Everything above is
  common to both and worth keeping textually identical so the two stay diffable;
  the project-database handlers are this repo's alone.

## Line endings

`cgate-server/DOCS.md` and `cgate-server/web/console.html` are stored with CRLF.
Python's text mode reads them as LF and writes LF back, silently reformatting
the whole file and burying the real change in a 300-line diff. After editing
either with a script, restore them:

```sh
perl -pi -e 's/(?<!\r)\n/\r\n/' cgate-server/DOCS.md
```

Check `git diff --stat` before committing; a small edit should be a small diff.

## Releasing

Branch, PR, merge — `main` is not committed to directly. Then:

1. Bump `version:` in `cgate-server/config.yaml` and add a `CHANGELOG.md` entry
   (both live under `cgate-server/`).
2. After merging: `git tag vX.Y.Z && git push origin vX.Y.Z`, then
   `gh release create vX.Y.Z`. Publishing the release triggers
   `.github/workflows/publish.yaml`.
3. Verify the images actually landed — the GitHub Packages UI lags badly:

```sh
repo=rbhr/aarch64-cgate-server
token=$(curl -s "https://ghcr.io/token?scope=repository:${repo}:pull&service=ghcr.io" | jq -r .token)
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $token" \
  -H "Accept: application/vnd.oci.image.index.v1+json" \
  "https://ghcr.io/v2/${repo}/manifests/X.Y.Z"
```

4. Home Assistant still shows the old version until Supervisor re-pulls its
   cached clone: **Settings → Add-ons → Add-on Store → ⋮ → Check for updates**,
   or `ha store reload`. The ⋮ menu on the add-on's own page does not do it.

## C-Gate behaviour worth knowing before changing code

- **Projects live wherever `project.default.dir` says.** That property is in
  `C-GateConfig.txt`, which persists in `/data/config` across updates, so it
  can differ between installations. `run.sh` sets it explicitly for this
  reason — do not infer the location from a fresh container, which only shows
  the default. The layout under it is `<dir>/<project>/<project>.db`, a SQLite
  file.
- A C-Bus Toolkit project is more than its `.db`: the dynamic labelling bitmaps
  and their index sit beside it and the database is useless without them.
  Toolkit's `.cbz` backup is a flat zip of that directory. C-Gate's own
  `PROJECT ARCHIVE` zip is different — one entry called `tagdb.db`.
- Access control (`config/access.txt`) matches connecting clients with
  `remote`, not `interface`. `interface` matches the local address a connection
  arrived on and never matches a client.
- **Do not set `ingress_entry`.** It was set to `/` here until 1.1.12, and
  every request arrived as `//` as a result. Supervisor builds the panel URL as

  ```python
  url = f"/api/hassio_ingress/{self.ingress_token}/"   # already ends in a slash
  if ATTR_INGRESS_ENTRY in self.data:
      return f"{url}{self.data[ATTR_INGRESS_ENTRY]}"   # appended RELATIVE
  ```

  so `ingress_entry: /` yields `.../<token>//`. Without the key the add-on is
  asked for `/`. The key is meant for add-ons whose entry point is a file, e.g.
  deconz's `ingress_entry: ingress.html` (note: no leading slash).

  Nothing was broken by it — `normalizePath` collapses `//` — which is why it
  survived so long here. It was not harmless elsewhere:
  `ha-app-OpenSprinkler-Server` copied the key from this repo for parity and
  shipped a broken panel, its firmware having no equivalent of `normalizePath`.

  `normalizePath` and its `//` test cases stay regardless. The collapse is now
  a guard against the key coming back, but the function's live job is stripping
  the ingress session prefix, which Supervisor does not always do — and
  `http.ServeMux` would answer 301 to a cleaned path and send the panel iframe
  to the Home Assistant dashboard. **Do not reintroduce a mux.**

## C-Gate's SSL interfaces and C-Bus Toolkit

C-Gate listens on 20023–20026 in the clear and on 20123–20126 over SSL; the
secure ports are `secure.port-base` and are always enabled inside the
container. The add-on published only the plaintext four until 1.1.12.

That mattered because **C-Bus Toolkit dials 20123 for a remote site and cannot
be told otherwise** — the remote port in `cgatesites.xml` is fixed at 20123. A
Toolkit that could not reach the add-on therefore produced no C-Gate log line
at all: the host RST the SYN and nothing ever arrived. If Toolkit cannot
connect, check the port is published before looking anywhere in C-Gate.

Reaching the port is only half of it. C-Gate still checks `config/access.txt`,
which matches clients with `remote`, and `run.sh` generates rules for Home
Assistant and the Supervisor network — not for a Toolkit PC elsewhere on the
LAN. That address has to be added to the add-on's `access_ips` option. In a
dotted quad any octet of 255 is a wildcard, so `192.168.1.255` allows a whole
/24.
