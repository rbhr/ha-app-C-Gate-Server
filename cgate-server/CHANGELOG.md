# Changelog

## 1.1.11

- **A C-Gate outage no longer hangs the web console.** The command session
  redialled C-Gate from inside `send()`, with its mutex held and no limit on
  how long it would keep trying. C-Gate is routinely away for minutes — a
  restart, a project reload, a reboot — and for that whole time every request
  queued behind that dial, including the ingress panel's own. Dialling is now
  one bounded attempt: a request made during an outage is answered with an
  error straight away, and a background goroutine does the waiting.
- **That goroutine also brings the session back on its own.** Recovery
  previously depended on somebody making a request; an idle console stayed
  disconnected indefinitely. It now reconnects within a few seconds of C-Gate
  returning, whether or not anyone is looking.
- **`/health` reports what is actually connected.** It answered a fixed
  `{"status":"ok"}` that never consulted C-Gate, so Home Assistant was told the
  add-on was fine while the command session was dead. It now carries the state
  of all three connections, and still returns 200 whenever the bridge is
  serving — it is what decides whether to restart, and failing it during
  C-Gate's minute-long cold start would turn a normal boot into a restart loop.
- **Added `/ready`**, which returns 503 until the command, event and status
  connections are all established. Gate on this, not `/health`, if you need
  C-Gate itself to be up. Neither tracks C-Gate's network state, so a project
  can still be mid-sync when `/ready` first passes.
- Restored the command-port heartbeat, dropped when this bridge diverged from
  its sibling in `C-Gate-Server-Container`: a periodic `noop` now detects a
  silently dropped session before a real command runs into it.

## 1.1.10

- **Fixed the event and status streams reconnecting every five minutes.** The
  bridge armed a five-minute read deadline before every read on C-Gate's event
  (20024) and status (20025) interfaces, on the assumption that silence meant a
  dead connection. It does not: both ends are inside this container, so a
  C-Gate that exits returns EOF immediately and the existing reconnect handles
  it. The deadline could therefore only ever fire on a healthy but quiet port —
  and both are legitimately quiet, since the event interface emits nothing at
  the default global-event-level and the status interface goes silent on an
  idle site. The result was a reconnect every 5m02s, forever.
- On the event stream that was log noise. On the status stream it also lost
  data: status changes arriving during the two-second reconnect gap were
  dropped, and that interface has no backfill. Busy sites never saw it, because
  traffic kept the deadline from firing. TCP keepalive remains as the backstop
  for a connection genuinely going away.
- Stream lines longer than 64KB no longer break the connection. The line
  scanner used its default limit and failed with `ErrTooLong`, which with the
  reconnect loop underneath it would have spun fast rather than slow; the limit
  is now 1MB.

## 1.1.9

- **Fixed projects becoming unfindable after the 1.1.8 upgrade.** C-Gate locates
  projects through its `project.default.dir` property, which persists in
  `/data/config/C-GateConfig.txt`. An installation whose C-Gate had been pointed
  at the tag directory kept looking there after 1.1.8 moved the databases,
  reporting `Unable to read path '/data/tag/<project>/<project>.db': file does
  not exist` and leaving the project unloadable.
- The add-on now sets `project.default.dir` explicitly on every start, so the
  location is the one it manages rather than whatever a previous version or a
  C-Bus Toolkit session left in the config file. This corrects 1.1.8's note that
  C-Gate never looks in the tag directory: it looks wherever that property
  says, which is exactly why it had to be set rather than assumed.
- The configured project is now loaded and started when C-Gate comes up, by
  setting `project.start`. A startup project that has already been set is left
  alone.
- Default configuration files are installed individually. They were only ever
  copied when the whole `/data/config` directory was missing, so a config
  directory that had lost a single file made the add-on exit during startup.

## 1.1.8

- **Fixed project databases never being visible to C-Gate, or persisted.** The
  add-on kept them in `/data/tag`, but C-Gate 3.8 reads projects from its
  `Projects` directory — a path built into `cgate.jar` — and treats the tag
  directory as the legacy XML tag database location only. A running add-on
  answered `project dir` with "no projects found", and anything C-Gate saved
  went to `/cgate/Projects` inside the container, so every project change was
  lost on the next restart or update.
- Projects now live in `/data/projects`, linked into place as C-Gate's
  `Projects` directory. Databases in `/data/tag` from earlier versions are moved
  across automatically on first start, and the add-on log lists the projects it
  can see once it has finished.
- Uploads therefore land where C-Gate reads them: after uploading, `project
  load` and `project start` now succeed rather than reporting that the database
  does not exist.
- A first upload no longer logs `Project not found` failures for the stop and
  close it does not need — C-Gate is asked whether it has the project open
  first.
- The `PROJECT` commands sent around an upload are allowed 20 seconds rather
  than 5 to answer, because stopping a started project means stopping its
  networks, which was timing out.

## 1.1.7

- Project uploads now take a whole project directory, not just the database.
  A C-Bus Toolkit `.cbz` backup, a `.zip`, or a `.tar`/`.tar.gz` of a project
  directory all work — which matters because the database is no use without the
  dynamic labelling bitmaps and index stored beside it, and the previous release
  moved only the `.db`.
- An upload is identified by its contents rather than its name, and the project
  name is read from the database inside the archive, so a Toolkit backup called
  `YELMAH_09_May_2025_2214_1.18.1.cbz` installs as project `YELMAH` with nothing
  to fill in.
- Archives are unpacked into a staging directory and only swapped in once
  complete; the previous project directory is kept as `<project>.bak`. Entries
  that would be written outside the project directory are refused, and an
  archive is capped at 4096 files and 256 MB unpacked.
- Each project can also be downloaded as a zip of its whole directory, in the
  same shape as a Toolkit backup. The list now shows the file count and the
  total size of the project rather than the size of the database alone.

## 1.1.6

- The web console can now download and upload project tag databases. **Tag
  database** in the console header lists the project databases in `/data/tag`
  and offers each for download; uploading a `.db` file replaces a project's
  database, which is how a project built in C-Bus Toolkit gets into the add-on
  without a shell.
- An upload is validated and written out in full before anything is replaced,
  the project is stopped and closed in C-Gate around the swap and loaded and
  started again afterwards, and the previous database is kept alongside as
  `<project>.db.bak`.

## 1.1.5

- Fixed Home Assistant being refused by C-Gate on a clean install. C-Gate's
  `interface` rules match the local address a connection arrives on, not the
  client, so the `interface 0.0.0.0` and `interface 172.30.32.2` rules the
  add-on shipped and appended never matched anything — only loopback had
  access, which is why the built-in web console worked but nothing else did.
  Clients are now allowed with `remote` rules.
- The add-on detects the address Home Assistant Core connects from on every
  start (Supervisor DNS, falling back to the container's default gateway) and
  grants it access, along with the Home Assistant host's own network addresses
  (read from the Supervisor API, which the add-on now requests), the Supervisor
  itself and the other add-ons on the Supervisor network.
- New **Allowed IP addresses** option for granting access to anything else,
  such as a PC running C-Bus Toolkit. Entries accept a `255` wildcard octet and
  an optional C-Gate access level.
- Managed rules are written to `/data/config/access.txt` between markers and
  rewritten on every start; rules added outside the block are preserved, and
  the stale rules from earlier versions are cleaned out on upgrade. The add-on
  log now prints the full rule list at startup.

## 1.1.4

- Fixed the ingress panel showing the Home Assistant dashboard instead of the
  C-Gate console. Supervisor builds `ingress_url` by joining the session path
  with the add-on's `ingress_entry`, so the request reaches the add-on as `//`.
  The web bridge routed on `http.ServeMux`, which answers `301` to the cleaned
  path when the request path is not already clean — sending the panel iframe to
  `/` on the Home Assistant origin. Routing is now explicit and never redirects.
- The web bridge now logs every request and the path it routed to, so ingress
  problems are visible in the add-on log.

## 1.1.3

- Updated C-Gate Server to v3.8.0 (build 2348)
- Fixed Home Assistant ingress: removed the `webui` entry that conflicted with
  ingress, and stopped the ingress path prefix being stripped twice
- Web console now derives its WebSocket and fetch URLs from `location.pathname`
  so they route correctly behind the ingress proxy
- Add-on `version` in config.yaml now tracks the release tag (it had been left
  at 1.0.0 since the initial release)

## 1.0.0

- Initial release
- C-Gate Server v3.7.0 (build 2285)
- Built-in web console with real-time event streaming
- Home Assistant ingress support
- Configurable project name, interface IP, and log level
- Persistent configuration and project databases
- Multi-architecture support (amd64, aarch64)
