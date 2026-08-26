# C-Gate Server — Home Assistant Add-on

[![Release](https://img.shields.io/github/v/release/rbhr/ha-app-C-Gate-Server?label=release)](https://github.com/rbhr/ha-app-C-Gate-Server/releases)
[![Architectures](https://img.shields.io/badge/arch-amd64%20%7C%20aarch64-blue)](https://github.com/rbhr/ha-app-C-Gate-Server/releases)

Runs Schneider Electric's SpaceLogic **C-Gate Server** as a Home Assistant
add-on, so a C-Bus installation can be driven from Home Assistant without a
separate machine sitting on the network.

C-Gate talks to a C-Bus network through a CNI, PCI, wireless gateway or serial
interface, and exposes command, event and status interfaces over TCP — which is
what the Home Assistant C-Bus integration connects to.

## Features

- **C-Gate Server 3.8.0** (build 2348), on amd64 and aarch64
- **Web console** on the add-on's ingress panel: live event and status
  streaming, command entry with history, and filterable log streams
- **Project databases in and out of the browser** — upload a C-Bus Toolkit
  `.cbz` backup, a `.zip` or `.tar` of a project directory, or a bare `.db`;
  download any project as either. No shell needed to get a Toolkit project into
  the add-on
- **Access control from the add-on options** — Home Assistant and the Supervisor
  network are granted access automatically; extra addresses, such as a PC
  running C-Bus Toolkit, are added by IP with an optional access level
- **Projects and configuration persist** in `/data` and survive add-on updates,
  and the configured project is loaded and started when C-Gate comes up

## Installation

1. **Settings → Add-ons → Add-on Store**
2. **⋮ → Repositories**, and paste
   `https://github.com/rbhr/ha-app-C-Gate-Server`
3. Find **C-Gate Server**, click **Install**, then configure and start it

Then point the C-Bus integration at `localhost` port `20023`.

Full configuration, the web console, ports and troubleshooting are documented in
**[the add-on documentation](cgate-server/DOCS.md)**, which Home Assistant also
renders in the add-on's Documentation tab.

## Ports

| Port  | Purpose                   |
|-------|---------------------------|
| 20023 | C-Gate command interface  |
| 20024 | C-Gate event interface    |
| 20025 | C-Gate status change port |
| 20026 | C-Gate config change port |
| 8980  | Web console (HTTP/WS)     |

Ports 20123–20126 are the SSL equivalents, disabled by default. The web console
is normally reached through ingress; port 8980 only needs exposing for direct
access.

## Add-ons

| Add-on | Description |
|--------|-------------|
| [C-Gate Server](cgate-server/) | SpaceLogic C-Gate Server for C-Bus |

## Licence

The add-on — the packaging, the startup script and the web console — is MIT
licensed; see [LICENSE](LICENSE).

**C-Gate itself is not.** It is proprietary software, copyright Schneider
Electric (Australia) Pty Ltd, redistributed here under the terms of the C-Gate
licence agreement included in the distribution
(`cgate-server/cgate-dist/C-Gate licence agreement.rtf`). This project is not
affiliated with or endorsed by Schneider Electric.
