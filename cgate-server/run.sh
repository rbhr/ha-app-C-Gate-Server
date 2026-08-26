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

# Install any default that is not there. An earlier version copied these only
# when the whole directory was missing, so a config directory that had lost a
# single file left the add-on dying on the next start.
mkdir -p /data/config
for DEFAULT in /cgate/defaults/*; do
    TARGET="/data/config/$(basename "$DEFAULT")"
    if [ ! -f "$TARGET" ]; then
        echo "Installing default $(basename "$DEFAULT")"
        cp "$DEFAULT" "$TARGET"
    fi
done

if [ ! -d /data/tag ]; then
    echo "First run: initialising /data/tag with defaults"
    mkdir -p /data/tag
fi

# --- Project databases ---
#
# C-Gate keeps projects in <cgate>/Projects/<name>/<name>.db, a path built into
# cgate.jar. The tag directory is the legacy XML tag database location and
# C-Gate never looks there for a project, so the project databases this add-on
# kept in /data/tag were invisible to it: `project dir` reported no projects,
# and anything C-Gate saved went to /cgate/Projects inside the container and
# was lost on the next restart. Projects now live in /data/projects, with the
# databases from earlier versions moved across on first run.

if [ ! -d /data/projects ]; then
    echo "First run: initialising /data/projects"
    mkdir -p /data/projects

    # Project directories earlier versions left in the tag directory.
    for DIR in /data/tag/*/; do
        NAME=$(basename "${DIR%/}")
        [ -f "${DIR}${NAME}.db" ] || continue
        echo "  moving project ${NAME} out of /data/tag"
        mv "${DIR%/}" /data/projects/
    done

    # A bare <name>.db there is a project too; C-Gate wants it in its own
    # directory.
    for FILE in /data/tag/*.db; do
        [ -f "$FILE" ] || continue
        NAME=$(basename "$FILE" .db)
        [ -d "/data/projects/${NAME}" ] && continue
        echo "  moving project ${NAME} out of /data/tag"
        mkdir -p "/data/projects/${NAME}"
        mv "$FILE" "/data/projects/${NAME}/${NAME}.db"
    done

    # Nothing to move on a clean install, so seed the shipped defaults.
    if [ -z "$(ls -A /data/projects 2>/dev/null)" ]; then
        echo "  seeding the default project"
        cp -r /cgate/tag-defaults/* /data/projects/ 2>/dev/null || true
    fi
fi

# --- Link persistent directories into C-Gate's expected locations ---

rm -rf /cgate/config /cgate/tag /cgate/Projects
ln -sf /data/config /cgate/config
ln -sf /data/tag /cgate/tag
ln -sf /data/projects /cgate/Projects
mkdir -p /cgate/logs

# Ensure the configured project's directory exists
mkdir -p "/data/projects/${PROJECT_NAME}"

echo "Projects on disk:"
for DIR in /data/projects/*/; do
    NAME=$(basename "${DIR%/}")
    if [ -f "${DIR}${NAME}.db" ]; then
        echo "  ${NAME} ($(ls -1 "$DIR" | wc -l | tr -d ' ') files)"
    fi
done

# --- Apply configuration ---

# Update log level in logback.xml
sed -i "s/level=\"[A-Z]*\"/level=\"${LOG_LEVEL}\"/" /data/config/logback.xml

# --- Point C-Gate at the project directory ---
#
# C-Gate finds projects under its project.default.dir property, which lives in
# C-GateConfig.txt and therefore persists in /data/config across updates. Its
# default is "Projects/", relative to C-Gate's own directory inside the
# container, but an installation may have been pointed somewhere else long ago
# and would then keep looking there. Set it explicitly on every start so the
# location is whatever this add-on manages, not whatever a previous version or
# a C-Bus Toolkit session left behind.
#
# C-Gate reads a partial config file and defaults everything it does not
# mention, so writing the file before C-Gate has ever run is safe.

CGATE_CONFIG=/data/config/C-GateConfig.txt

# set_cgate_property KEY VALUE — replace the property in place, or append it.
set_cgate_property() {
    if [ -f "$CGATE_CONFIG" ] && grep -q "^$1=" "$CGATE_CONFIG"; then
        # '|' as the delimiter: it cannot appear in the paths set here
        sed -i "s|^$1=.*|$1=$2|" "$CGATE_CONFIG"
    else
        printf '%s=%s\n' "$1" "$2" >> "$CGATE_CONFIG"
    fi
}

set_cgate_property project.default.dir "/data/projects/"
set_cgate_property project.default.archive-dir "/data/projects/archived/"
echo "  Projects:  $(awk -F= '/^project.default.dir=/{print $2; exit}' "$CGATE_CONFIG")"

# Load and start the configured project at boot, but never override a startup
# project that has been set deliberately.
if ! grep -q "^project.start=." "$CGATE_CONFIG" 2>/dev/null; then
    set_cgate_property project.start "${PROJECT_NAME}"
    echo "  Autostart: ${PROJECT_NAME}"
else
    echo "  Autostart: $(awk -F= '/^project.start=/{print $2; exit}' "$CGATE_CONFIG") (from C-GateConfig.txt)"
fi

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

# The bridge serves the project databases for download and upload, so it needs
# to know where they live and which project is in use.
export CGATE_PROJECTS_DIR=/data/projects
export CGATE_PROJECT="${PROJECT_NAME}"

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
