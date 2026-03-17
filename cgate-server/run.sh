#!/bin/sh
set -e

OPTIONS_FILE="/data/options.json"

# Parse Home Assistant add-on options
PROJECT_NAME=$(jq -r '.project_name // "HOME"' "$OPTIONS_FILE")
INTERFACE_IP=$(jq -r '.interface_ip // ""' "$OPTIONS_FILE")
LOG_LEVEL=$(jq -r '.log_level // "DEBUG"' "$OPTIONS_FILE")
CGATE_ARGS=$(jq -r '.cgate_args // ""' "$OPTIONS_FILE")

echo "C-Gate Server starting..."
echo "  Project:   ${PROJECT_NAME}"
echo "  Interface: ${INTERFACE_IP:-local}"
echo "  Log level: ${LOG_LEVEL}"

# --- Initialise persistent storage on first run ---

if [ ! -d /data/config ]; then
    echo "First run: initialising /data/config with defaults"
    mkdir -p /data/config
    cp /cgate/defaults/* /data/config/
fi

if [ ! -d /data/tag ]; then
    echo "First run: initialising /data/tag with defaults"
    mkdir -p /data/tag
    cp -r /cgate/tag-defaults/* /data/tag/ 2>/dev/null || true
fi

# --- Link persistent directories into C-Gate's expected locations ---

rm -rf /cgate/config /cgate/tag
ln -sf /data/config /cgate/config
ln -sf /data/tag /cgate/tag
mkdir -p /cgate/logs

# Ensure the configured project database directory exists
mkdir -p "/data/tag/${PROJECT_NAME}"

# --- Apply configuration ---

# Update log level in logback.xml
sed -i "s/level=\"[A-Z]*\"/level=\"${LOG_LEVEL}\"/" /data/config/logback.xml

# Ensure Home Assistant ingress proxy IP is allowed
if ! grep -q "172.30.32.2" /data/config/access.txt; then
    echo "interface 172.30.32.2 Program" >> /data/config/access.txt
fi

# --- Start Go web bridge with auto-restart ---

(
    while true; do
        /cgate/cgate-web
        echo "cgate-web exited ($?) — restarting in 2s" >&2
        sleep 2
    done
) &

# --- Launch C-Gate as PID 1 ---

exec java \
    -Djava.library.path=. \
    -Dlogback.configurationFile=/cgate/config/logback.xml \
    -Xms64M \
    -Xmx256M \
    -jar cgate.jar \
    -s
