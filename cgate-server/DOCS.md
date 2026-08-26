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
`/data/tag/<project_name>/`.

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
`/data/tag`, with how many files it holds, its total size, and when its database
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
| 8980  | Web Console (HTTP/WS)      |

Ports 20123–20126 are the SSL equivalents (disabled by default).

## Home Assistant C-Bus Integration

After starting the add-on, configure the C-Bus integration to connect to
C-Gate at `localhost` on port `20023`.

## Persistent Data

Configuration and project databases are stored in `/data/` and persist across
add-on updates and restarts. On first run, default configuration files are
copied automatically.

- `/data/config/` — access.txt, C-groups.txt, logback.xml
- `/data/tag/` — C-Gate project databases, as `<project>/<project>.db`

Project databases can be backed up and replaced from the web console — see
**Project tag databases** above.

## Troubleshooting

- Check the add-on **Log** tab for C-Gate startup messages.
- Use the web console to send `version` to verify C-Gate is responding.
- Ensure your C-Bus interface IP is correct and reachable from the HA host.
- If C-Gate fails to start, try increasing the log level to `DEBUG` or `TRACE`.
- `Access control refused` in the log means the connecting address is not in
  the access control list — add it under **Allowed IP addresses**. The add-on
  log prints the full rule list on every start.
