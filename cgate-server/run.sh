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

# --- Access control ---
#
# C-Gate checks every connection against config/access.txt. The `interface`
# keyword matches the local address a connection arrives on, so it never
# matches a client; connecting clients are matched with `remote`. In a dotted
# quad any octet set to 255 is a wildcard, so 172.30.33.255 matches every
# add-on on the Supervisor network.
#
# Everything between the markers below is regenerated on every start. Rules
# written outside the block are left alone.

ACCESS_FILE="/data/config/access.txt"
ACCESS_BEGIN="## BEGIN Home Assistant managed rules - regenerated on every start"
ACCESS_END="## END Home Assistant managed rules"

# Home Assistant Core runs with host networking, so its connections reach the
# add-on from the Supervisor bridge gateway. Ask Supervisor's DNS for it and
# fall back to our own default gateway, then to the documented address.
HA_IP=$(getent hosts homeassistant.local.hass.io 2>/dev/null | awk '{print $1; exit}')
if [ -z "$HA_IP" ]; then
    GW_HEX=$(awk '$2 == "00000000" && $8 == "00000000" { print $3; exit }' /proc/net/route 2>/dev/null)
    if [ -n "$GW_HEX" ]; then
        # /proc/net/route stores the gateway as a little-endian hex word
        set -- $(echo "$GW_HEX" | sed 's/../& /g')
        HA_IP="$((0x$4)).$((0x$3)).$((0x$2)).$((0x$1))"
    fi
fi
: "${HA_IP:=172.30.32.1}"
echo "  HA host:   ${HA_IP}"

# The Home Assistant host's own LAN addresses, so connections that arrive on
# them rather than over the Supervisor bridge are allowed too. Ignore loopback
# and the Supervisor network, which are covered above.
HA_LAN_IPS=""
if [ -n "${SUPERVISOR_TOKEN:-}" ]; then
    HA_LAN_IPS=$(curl -sf -m 5 -H "Authorization: Bearer ${SUPERVISOR_TOKEN}" \
        http://supervisor/network/info 2>/dev/null |
        jq -r '[.data.interfaces[]?.ipv4?.address[]?] | .[]' 2>/dev/null |
        sed 's#/.*##' |
        grep -v -e '^127\.' -e '^172\.30\.3[23]\.' || true)
    [ -n "$HA_LAN_IPS" ] && echo "  HA LAN:    $(echo "$HA_LAN_IPS" | tr '\n' ' ')"
fi

ACCESS_TMP=$(mktemp)

# Keep the existing file minus the managed block and minus rules earlier
# versions of this script appended, which used the wrong keyword and never
# matched anything.
if [ -f "$ACCESS_FILE" ]; then
    awk -v b="$ACCESS_BEGIN" -v e="$ACCESS_END" '
        { sub(/\r$/, "") }   # files from earlier versions have CRLF endings
        $0 == b { skip = 1; next }
        $0 == e { skip = 0; next }
        skip { next }
        $0 == "interface 172.30.32.2 Program" { next }
        $0 == "interface 0.0.0.0 Program" { next }
        $0 == "## Modified for containerised deployment - allows programming from any IP" { next }
        { print }
    ' "$ACCESS_FILE" > "$ACCESS_TMP"
fi

{
    echo "$ACCESS_BEGIN"
    echo "## Home Assistant Core"
    echo "remote ${HA_IP} Program"
    if [ "$HA_IP" != "172.30.32.1" ]; then
        echo "remote 172.30.32.1 Program"
    fi
    if [ -n "$HA_LAN_IPS" ]; then
        echo "## Home Assistant host interfaces"
        echo "$HA_LAN_IPS" | while read -r LAN_IP; do
            [ -n "$LAN_IP" ] || continue
            echo "remote ${LAN_IP} Program"
        done
    fi
    echo "## Supervisor (ingress, add-on API)"
    echo "remote 172.30.32.2 Program"
    echo "## Other add-ons on the Supervisor network"
    echo "remote 172.30.33.255 Program"

    # Extra addresses from the add-on options. Each entry is an address, or an
    # address followed by a C-Gate access level; the default level is Program.
    jq -r '(.access_ips // [])[] | select(. != null)' "$OPTIONS_FILE" |
    while read -r ENTRY; do
        ADDRESS=$(echo "$ENTRY" | awk '{print $1}')
        [ -n "$ADDRESS" ] || continue
        LEVEL=$(echo "$ENTRY" | awk '{print ($2 == "" ? "Program" : $2)}')
        echo "## Configured in the add-on options"
        echo "remote ${ADDRESS} ${LEVEL}"
    done

    echo "$ACCESS_END"
} >> "$ACCESS_TMP"

cat "$ACCESS_TMP" > "$ACCESS_FILE"
rm -f "$ACCESS_TMP"

echo "Access control rules:"
awk '$1 == "interface" || $1 == "remote" || $1 == "user" { print "  " $0 }' "$ACCESS_FILE"

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
