# Changelog

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
