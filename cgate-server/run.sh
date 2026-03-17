#!/bin/sh
set -e

OPTIONS_FILE="/data/options.json"

# Parse Home Assistant add-on options (no jq — restricted in HA containers)
json_value() {
    sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$OPTIONS_FILE"
}
PROJECT_NAME=$(json_value project_name)
PROJECT_NAME="${PROJECT_NAME:-HOME}"
INTERFACE_IP=$(json_value interface_ip)
LOG_LEVEL=$(json_value log_level)
LOG_LEVEL="${LOG_LEVEL:-DEBUG}"
CGATE_ARGS=$(json_value cgate_args)

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

# --- Build C-Gate command line ---

CMD_ARGS="-s"
if [ -n "$INTERFACE_IP" ]; then
    CMD_ARGS="$CMD_ARGS -connect $INTERFACE_IP"
fi
CMD_ARGS="$CMD_ARGS -project $PROJECT_NAME"
if [ -n "$CGATE_ARGS" ]; then
    CMD_ARGS="$CMD_ARGS $CGATE_ARGS"
fi

# --- Launch C-Gate as PID 1 ---

exec java \
    -Djava.library.path=. \
    -Dlogback.configurationFile=/cgate/config/logback.xml \
    -Xms64M \
    -Xmx256M \
    -jar cgate.jar \
    $CMD_ARGS
