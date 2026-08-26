# Changelog

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
