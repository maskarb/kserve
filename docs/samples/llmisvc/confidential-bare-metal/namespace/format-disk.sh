#!/bin/bash
set -eu

DEVICE=/dev/block-device
# Override this together with the URI in the sealed secret for another KBS key.
: "${LUKS_RESOURCE_ID:?LUKS_RESOURCE_ID is required}"
if [[ ! "$LUKS_RESOURCE_ID" =~ ^kbs:///([a-zA-Z0-9_.-]+)/([a-zA-Z0-9_.-]+)/([a-zA-Z0-9_.-]+)$ ]]; then
    echo "cc-storage: invalid LUKS_RESOURCE_ID" >&2
    exit 1
fi
resource_path="${BASH_REMATCH[1]}/${BASH_REMATCH[2]}/${BASH_REMATCH[3]}"

fetch_pass() {
    exec 3<>"/dev/tcp/127.0.0.1/${CDH_PORT:-8006}" || return 1
    printf 'GET /cdh/resource/%s HTTP/1.0\r\nHost: 127.0.0.1\r\n\r\n' "$resource_path" >&3
    local status line body length=""
    if ! IFS= read -r -t 30 status <&3; then
        exec 3<&- 3>&-
        return 1
    fi
    status=${status%$'\r'}
    while true; do
        if ! IFS= read -r -t 30 line <&3; then
            exec 3<&- 3>&-
            return 1
        fi
        line=${line%$'\r'}
        [ -z "$line" ] && break
        case "$line" in
            [Cc]ontent-[Ll]ength:\ *) length=${line#*: } ;;
        esac
    done
    case "$status" in
        'HTTP/1.0 200 '*|'HTTP/1.1 200 '*) ;;
        *) exec 3<&- 3>&-; return 1 ;;
    esac
    if [ -n "$length" ]; then
        if [[ ! "$length" =~ ^[0-9]{1,4}$ ]] || ((10#$length < 1 || 10#$length > 4096)); then
            exec 3<&- 3>&-
            return 1
        fi
        # Refuse a truncated or timed-out response instead of using a partial key.
        if ! IFS= read -r -N "$length" -t 30 body <&3; then
            exec 3<&- 3>&-
            return 1
        fi
    else
        # HTTP/1.0 may delimit the body by closing the connection.
        if IFS= read -r -N 4097 -t 30 body <&3; then
            exec 3<&- 3>&-
            return 1
        else
            status=$?
            if [ "$status" -ne 1 ]; then
                exec 3<&- 3>&-
                return 1
            fi
        fi
    fi
    exec 3<&- 3>&-
    case "$body" in
        ''|sealed.*) return 1 ;;
    esac
    printf '%s' "$body"
}

case "${PASS:-}" in
    ''|sealed.*)
        PASS=""
        for ((attempt = 0; attempt < 90; attempt++)); do
            if PASS=$(fetch_pass); then
                break
            fi
            sleep 2
        done
        ;;
esac
case "${PASS:-}" in
    ''|sealed.*)
        echo "cc-storage: could not retrieve the LUKS passphrase from CDH" >&2
        exit 1
        ;;
esac
case "$PASS" in
    *$'\n'*|*$'\r'*)
        echo "cc-storage: LUKS passphrase must be a single-line text value" >&2
        exit 1
        ;;
esac
export PASS

command -v cryptsetup >/dev/null
if cryptsetup isLuks "$DEVICE"; then
    if ! printf '%s' "$PASS" | cryptsetup --key-file=- luksOpen --test-passphrase "$DEVICE"; then
        echo "cc-storage: cannot unlock the existing LUKS volume; leaving the device intact" >&2
        exit 1
    fi
else
    status=$?
    if [ "$status" -ne 1 ]; then
        echo "cc-storage: cannot inspect the block device; leaving it intact" >&2
        exit 1
    fi
fi
exec /usr/local/bin/luks-helper format-disk
