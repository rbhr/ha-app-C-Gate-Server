#!/bin/sh
set -e

OPTIONS_FILE="/data/options.json"

# Parse Home Assistant add-on options
PROJECT_NAME=$(jq -r '.project_name // "HOME"' "$OPTIONS_FILE")
LOG_LEVEL=$(jq -r '.log_level // "DEBUG"' "$OPTIONS_FILE")
CGATE_ARGS=$(jq -r '.cgate_args // ""' "$OPTIONS_FILE")

case "$PROJECT_NAME" in
    ""|*[!A-Za-z0-9_-]*) echo "Invalid project_name: use letters, digits, - and _" >&2; exit 1 ;;
esac
[ "${#PROJECT_NAME}" -le 32 ] || { echo "project_name is longer than 32 characters" >&2; exit 1; }
case "$LOG_LEVEL" in TRACE|DEBUG|INFO|WARN|ERROR) ;; *) echo "Invalid log_level" >&2; exit 1 ;; esac
if [ -n "$(jq -r '.interface_ip // ""' "$OPTIONS_FILE")" ]; then
    echo "The retired interface_ip option had no effect. Configure C-Bus interfaces in the project using Toolkit." >&2
fi

echo "C-Gate Server starting..."
echo "  Project:   ${PROJECT_NAME}"
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

mkdir -p /data/projects
# Recover interrupted directory swaps before Java can load either copy.
CGATE_PROJECTS_DIR=/data/projects /cgate/cgate-web -recover

# Migration is checked per project on every start. Directory creation is not
# a completion marker; an interruption after mkdir is safe to retry.
for DIR in /data/tag/*/; do
    NAME=$(basename "${DIR%/}")
    [ -f "${DIR}${NAME}.db" ] || continue
    TARGET="/data/projects/${NAME}"
    if [ -d "$TARGET" ] && [ -z "$(ls -A "$TARGET")" ]; then rmdir "$TARGET"; fi
    if [ -e "$TARGET" ]; then
        echo "Migration conflict for ${NAME}: leaving the legacy project in /data/tag for review" >&2
        continue
    fi
    echo "  moving project ${NAME} out of /data/tag"
    mv "${DIR%/}" "$TARGET"
done
for FILE in /data/tag/*.db; do
    [ -f "$FILE" ] || continue
    NAME=$(basename "$FILE" .db)
    TARGET="/data/projects/${NAME}"
    if [ -e "${TARGET}/${NAME}.db" ]; then
        echo "Migration conflict for ${NAME}: leaving the legacy database in /data/tag for review" >&2
        continue
    fi
    mkdir -p "$TARGET"
    echo "  moving project ${NAME} out of /data/tag"
    mv "$FILE" "${TARGET}/${NAME}.db"
done

# Seed only when no project database exists. Install a whole default directory
# by rename so an interrupted copy is never mistaken for a completed seed.
HAVE_PROJECT=false
for DIR in /data/projects/*/; do
    NAME=$(basename "${DIR%/}")
    [ ! -f "${DIR}${NAME}.db" ] || HAVE_PROJECT=true
done
if [ "$HAVE_PROJECT" = false ]; then
    for DEFAULT in /cgate/tag-defaults/*/; do
        [ -d "$DEFAULT" ] || continue
        NAME=$(basename "${DEFAULT%/}")
        TARGET="/data/projects/${NAME}"
        if [ -d "$TARGET" ] && [ -z "$(ls -A "$TARGET")" ]; then rmdir "$TARGET"; fi
        if [ -e "$TARGET" ]; then
            echo "Cannot seed ${NAME}: a nonempty project directory already exists" >&2
            exit 1
        fi
        STAGING=$(mktemp -d /data/projects/.seed-XXXXXX)
        cp -R "${DEFAULT}." "$STAGING/"
        sync
        mv "$STAGING" "$TARGET"
        sync
    done
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
    PROPERTY_TMP=$(mktemp /data/config/.property-XXXXXX)
    if [ -f "$CGATE_CONFIG" ]; then
        awk -F= -v key="$1" -v value="$2" '
            $1 == key { if (!written) print key "=" value; written=1; next }
            { print }
            END { if (!written) print key "=" value }
        ' "$CGATE_CONFIG" > "$PROPERTY_TMP"
    else
        printf '%s=%s\n' "$1" "$2" > "$PROPERTY_TMP"
    fi
    mv "$PROPERTY_TMP" "$CGATE_CONFIG"
}

set_cgate_property project.default.dir "/data/projects/"
set_cgate_property project.default.archive-dir "/data/projects/archived/"
echo "  Projects:  $(awk -F= '/^project.default.dir=/{print $2; exit}' "$CGATE_CONFIG")"

# Track the last value written by the add-on. Changes made directly in
# C-GateConfig.txt remain overrides; later option changes update managed values.
START_OWNER=/data/config/.managed-project-start
CURRENT_START=$(awk -F= '$1 == "project.start" {sub(/\r$/, "", $2); print $2; exit}' "$CGATE_CONFIG")
IS_MANAGED=false
if [ -f "$START_OWNER" ] && grep -Fxq -- "$CURRENT_START" "$START_OWNER"; then IS_MANAGED=true; fi
if [ -z "$CURRENT_START" ] || [ "$IS_MANAGED" = true ] ||
   { [ ! -f "$START_OWNER" ] && [ "$CURRENT_START" = "$PROJECT_NAME" ]; }; then
    # Record both values before changing the property. A crash between the
    # two files can then be reconciled as a managed update on the next boot.
    printf '%s\n%s\n' "$CURRENT_START" "$PROJECT_NAME" > "${START_OWNER}.tmp"
    sync
    mv "${START_OWNER}.tmp" "$START_OWNER"
    sync
    set_cgate_property project.start "$PROJECT_NAME"
    sync
    printf '%s\n' "$PROJECT_NAME" > "${START_OWNER}.tmp"
    mv "${START_OWNER}.tmp" "$START_OWNER"
    echo "  Autostart: ${PROJECT_NAME} (managed by project_name)"
else
    echo "  Autostart: ${CURRENT_START} (preserved override in C-GateConfig.txt)"
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
        # A failing command must be tested explicitly under set -e, otherwise
        # this subshell exits and leaves Java running without the bridge.
        if /cgate/cgate-web; then
            STATUS=0
        else
            STATUS=$?
        fi
        echo "cgate-web exited (${STATUS}) — restarting in 2s" >&2
        sleep 2
    done
) &

# --- Launch C-Gate as PID 1 ---

# cgate_args is whitespace-separated C-Gate arguments, never shell code.
# Disable filename expansion and pass each argument literally; do not eval it.
set -f
set -- $CGATE_ARGS
set +f

exec java \
    -Djava.library.path=. \
    -Dlogback.configurationFile=/cgate/config/logback.xml \
    -Xms64M \
    -Xmx256M \
    -jar cgate.jar \
    -s "$@"
