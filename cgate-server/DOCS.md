# C-Gate Server

This add-on runs the Schneider Electric SpaceLogic C-Gate Server (v3.7.0) for
managing C-Bus home automation networks from Home Assistant.

## What is C-Gate?

C-Gate is a TCP server that communicates with C-Bus networks via a C-Bus
interface (CNI, PCI, wireless gateway, or serial). It provides command, event,
and status interfaces that the Home Assistant C-Bus integration connects to.

This add-on also includes a built-in **web console** for direct interaction
with C-Gate — useful for debugging, diagnostics, and manual control.

## Installation

1. Add this repository to Home Assistant:
   **Settings → Add-ons → Add-on Store → ⋮ → Repositories** and paste:
   `https://github.com/rbhr/ha-app-C-Gate-Server`
2. Find **C-Gate Server** in the store and click **Install**.
3. Configure the add-on (see below) and click **Start**.

## Configuration

### Project Name

The C-Gate project name corresponding to your C-Bus installation. The default
is `HOME`. Each project stores its configuration in a separate database under
`/data/projects/<project_name>/`.

### Interface IP

The IP address of your C-Bus network interface (e.g. a CNI at `192.168.1.10`).
Leave empty if using a directly connected serial or USB interface.

### Allowed IP Addresses

C-Gate refuses connections from any address that is not in its access control
list, so this option controls who may talk to it besides Home Assistant.

Home Assistant itself never needs to be listed. On every start the add-on
detects and grants access to:

- the address Home Assistant Core connects from (the Supervisor bridge
  gateway, since Core uses host networking),
- the Home Assistant host's own network addresses, read from the Supervisor,
- the Supervisor itself, which serves the ingress panel,
- the other add-ons on the Supervisor network.

The add-on log lists every rule at startup, so the **Log** tab shows exactly
what is allowed.

Add an entry per address that needs access, for example a PC running C-Bus
Toolkit:

```yaml
access_ips:
  - 192.168.1.50
```

- Any part of an address set to `255` is a wildcard, so `192.168.1.255` allows
  the whole subnet, and `255.255.255.255` allows any address at all (useful to
  confirm access control is the problem, but not a setting to leave in place).
- A hostname works in place of an address; it is resolved when a connection is
  made, not when the add-on starts.
- Entries grant full `Program` access. To grant less, put a C-Gate access level
  after the address — `none`, `connect`, `monitor`, `operate`, `admin`,
  `program` or `debug`:

```yaml
access_ips:
  - 192.168.1.50 monitor
```

The add-on writes these rules into `/data/config/access.txt` between
`## BEGIN Home Assistant managed rules` and `## END Home Assistant managed
rules`, rewriting that block on every start. Rules you add to the file outside
the block are preserved.

### Log Level

Controls the verbosity of C-Gate logging. Options: `TRACE`, `DEBUG`, `INFO`,
`WARN`, `ERROR`. Default is `DEBUG`.

### Additional Arguments

Advanced: extra command-line arguments passed directly to the C-Gate Java
process. Most users should leave this empty.

## Web Console

The add-on includes a built-in web console accessible via:

- **Ingress**: Click "OPEN WEB UI" in the add-on panel (recommended).
- **Direct access**: Enable port 8980 in the add-on's Network configuration.

The console provides:

- Real-time streaming of C-Bus events and status changes
- Command entry for sending C-Gate commands
- Filterable log streams (events, status, commands, responses)
- Download and upload of project tag databases

### Project tag databases

**Tag database** in the console header opens a panel listing every project in
`/data/projects`, with how many files it holds, its total size, and when its database
was last written.

**Download** offers each project as **.db**, the database on its own, and — for a
project with more than the database in its directory — **.zip**, the whole
directory. The zip has the same shape as Toolkit's `.cbz`, so it can go straight
back in. C-Gate holds a loaded project in memory and only writes it to disk
when told to, so send `project save` in the console first if the project has
been changed since it was loaded; otherwise the download is the last saved
copy.

**Upload** puts a project from your PC into the add-on, which is how a project
built in C-Bus Toolkit gets in. It takes any of:

| File | What it is |
|------|------------|
| `.cbz` | A Toolkit backup — a zip of the whole project directory. The usual choice. |
| `.zip` | Any zip of a project directory. |
| `.tar`, `.tar.gz` | The project directory as copied off another machine. |
| `.db` | Just the database, leaving anything else in the project directory alone. |

The upload is identified by its contents, not its name, so Toolkit's
`YELMAH_09_May_2025_2214_1.18.1.cbz` needs no renaming. **The project name comes
from the database inside the archive** — an archive holding `YELMAH.db` installs
as project `YELMAH` — so the project box can be left empty. It is needed only
for an archive made by C-Gate's own `PROJECT ARCHIVE` command, which names the
database `tagdb.db` and so does not say which project it is.

An archive replaces the whole project directory, because the database is no use
without the dynamic labelling bitmaps and index stored beside it. A `.db` on its
own replaces only the database.

Either way, uploading:

1. checks the file and unpacks it into a staging directory, so nothing in place
   is touched until a complete project has landed on disk,
2. tells C-Gate to `project stop` and `project close` the project, so it is not
   holding the old copy in memory,
3. moves the existing project aside — to `<project>.bak/` for an archive, or
   `<project>.db.bak` for a bare database — and installs the new one,
4. tells C-Gate to `project load` and `project start` the project again.

C-Gate's replies to those commands appear in the console log. Uploading a
project that is not the one in **Project Name** installs it but leaves the
configured project running.

Only the most recent backup is kept, so download the current project before
uploading twice over.

### Limits

An upload may be up to 64 MB and expand to no more than 256 MB across at most
4096 files. Entries that would be written outside the project directory, and
anything that is not a plain file, are refused.

## Ports

| Port  | Purpose                    |
|-------|----------------------------|
| 20023 | C-Gate Command Interface   |
| 20024 | C-Gate Event Interface     |
| 20025 | C-Gate Status Change Port  |
| 20026 | C-Gate Config Change Port  |
| 20123 | SSL Command Interface      |
| 20124 | SSL Event Interface        |
| 20125 | SSL Status Change Port     |
| 20126 | SSL Config Change Port     |
| 8980  | Web Console (HTTP/WS)      |

20123–20126 are the same four interfaces over TLS. C-Gate always listens on
them; before 1.1.12 the add-on simply did not publish them, so connections
were refused by the host before C-Gate ever saw them.

### Connecting C-Bus Toolkit

Toolkit talks to a remote C-Gate over **20123**, and that is effectively fixed
— the remote port in its `cgatesites.xml` is 20123 and there is no supported
way to point a remote site at 20023 instead.

Two things have to be true, and only the first of them produces a log line:

1. **The port has to be published.** It is, from 1.1.12 on. If you have
   customised the add-on's network settings, check 20123 still has a host port
   in **Settings → Add-ons → C-Gate Server → Configuration → Network**. An
   unpublished port means the host answers Toolkit's connection with an
   immediate reset, and **nothing appears in the C-Gate log at all** — the
   connection never reached it. A silent log here means the port, not C-Gate.
2. **The Toolkit PC has to be in the access control list.** Add its address to
   [Allowed IP Addresses](#allowed-ip-addresses); the automatic rules cover
   Home Assistant and the Supervisor network, not another machine on the LAN.
   Refusals of this kind *are* logged.

C-Gate presents its Schneider factory certificate on the SSL ports and does not
ask for a client certificate. Toolkit ships the matching trust, so no
certificate setup is needed.

### Health checks

Two endpoints on port 8980 report the state of the bridge's connections to
C-Gate. Both return the same body:

```json
{"status":"ok","connections":{"command":true,"event":true,"status":true}}
```

`status` is `ok` only when all three connections are up, and `degraded`
otherwise.

- **`/health`** always answers 200 while the add-on is serving, even with
  C-Gate unreachable. It is what decides whether the add-on gets restarted, and
  C-Gate takes up to a minute to sync its networks on a cold start, so failing
  it during that window would turn a normal boot into a restart loop. Read
  `status` in the body to tell a healthy bridge from one that has lost C-Gate.
- **`/ready`** answers 503 until the command, event and status connections are
  all established, then 200. Gate on this rather than `/health` if you need
  C-Gate itself to be up: it lets a client hold its first poll instead of
  retrying into `408 Operation failed` while C-Gate is still starting.

Neither tracks C-Gate's *network* state, so a project can still be mid-sync
when `/ready` first passes.

## Home Assistant C-Bus Integration

After starting the add-on, configure the C-Bus integration to connect to
C-Gate at `localhost` on port `20023`.

## Persistent Data

Configuration and project databases are stored in `/data/` and persist across
add-on updates and restarts. On first run, default configuration files are
copied automatically.

- `/data/config/` — access.txt, C-groups.txt, logback.xml, and C-Gate's own
  C-GateConfig.txt
- `/data/projects/` — C-Gate projects, as `<project>/<project>.db` plus
  whatever else the project keeps beside its database
- `/data/tag/` — C-Gate's legacy XML tag database directory

Project databases can be backed up and replaced from the web console — see
**Project tag databases** above.

### Where C-Gate looks for projects

C-Gate finds projects under its `project.default.dir` property, which lives in
`C-GateConfig.txt` and so persists in `/data/config` across updates. Its own
default is `Projects/`, relative to C-Gate's directory inside the container —
where nothing survives a restart — but an installation may have been pointed
somewhere else at some point and will then keep looking there.

The add-on sets that property to `/data/projects/` on every start, so the
location is always the one it manages. The startup log shows it:

```
Projects:  /data/projects/
Autostart: HOME
```

It also sets `project.start` so the configured project is loaded and started
when C-Gate comes up — unless a startup project has already been set, which is
left alone.

### Upgrading from 1.1.7 or earlier

Project databases used to live in `/data/tag`. They are moved to
`/data/projects` on the first start after upgrading, and C-Gate is pointed at
the new location in the same start, so there is nothing to do by hand. The move
is logged and nothing is deleted from `/data/tag`.

## Troubleshooting

- Check the add-on **Log** tab for C-Gate startup messages.
- Use the web console to send `version` to verify C-Gate is responding.
- `curl http://<ha-host>:8980/ready` reports which of the three C-Gate
  connections the bridge is holding, if port 8980 is enabled under
  **Network** — see **Health checks** above.
- Ensure your C-Bus interface IP is correct and reachable from the HA host.
- If C-Gate fails to start, try increasing the log level to `DEBUG` or `TRACE`.
- `Access control refused` in the log means the connecting address is not in
  the access control list — add it under **Allowed IP addresses**. The add-on
  log prints the full rule list on every start.
