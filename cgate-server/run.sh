#!/bin/sh
set -e

OPTIONS_FILE="/data/options.json"

# Parse Home Assistant add-on options using pure shell builtins
# (HA container security blocks external binaries like jq, sed, grep)
json_value() {
    key="$1"
    while IFS= read -r line; do
        case "$line" in
            *"\"${key}\""*)
                # strip everything up to and including the colon
                val="${line#*:}"
                # strip leading whitespace and quotes
                val="${val#"${val%%[! ]*}"}"
                val="${val#\"}"
                # strip trailing quote, comma, whitespace
                val="${val%\"*}"
                echo "$val"
                return
                ;;
        esac
    done < "$OPTIONS_FILE"
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

# Update log level in logback.xml (pure shell, no sed)
if [ -f /data/config/logback.xml ]; then
    tmpfile="/data/config/logback.xml.tmp"
    while IFS= read -r line; do
        case "$line" in
            *'level="'*'"'*)
                # Replace level="WHATEVER" with configured level
                prefix="${line%%level=\"*}"
                suffix="${line#*level=\"}"
                suffix="${suffix#*\"}"
                echo "${prefix}level=\"${LOG_LEVEL}\"${suffix}"
                ;;
            *)
                echo "$line"
                ;;
        esac
    done < /data/config/logback.xml > "$tmpfile"
    mv "$tmpfile" /data/config/logback.xml
fi

# Ensure Home Assistant ingress proxy IP is allowed
access_file="/data/config/access.txt"
found=0
if [ -f "$access_file" ]; then
    while IFS= read -r line; do
        case "$line" in
            *172.30.32.2*) found=1; break ;;
        esac
    done < "$access_file"
fi
if [ "$found" = "0" ]; then
    echo "interface 172.30.32.2 Program" >> "$access_file"
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
