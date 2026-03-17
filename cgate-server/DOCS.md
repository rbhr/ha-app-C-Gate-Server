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
- `/data/tag/` — C-Gate project databases

## Troubleshooting

- Check the add-on **Log** tab for C-Gate startup messages.
- Use the web console to send `version` to verify C-Gate is responding.
- Ensure your C-Bus interface IP is correct and reachable from the HA host.
- If C-Gate fails to start, try increasing the log level to `DEBUG` or `TRACE`.
