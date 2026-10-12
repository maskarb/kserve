#!/bin/sh
# Sourced by both the initializer wrapper and the inherited vLLM Bash launcher.
cc_storage_link() {
    DATA_DIR="${DATA_DIR:-/mnt/models}"
    cc_storage_pid_file="${CC_STORAGE_PID_FILE:-/dev/shm/luks-helper.pid}"
    cc_storage_max_wait="${CC_STORAGE_MAX_WAIT:-900}"
    cc_storage_waited=0

    while [ "$cc_storage_waited" -lt "$cc_storage_max_wait" ]; do
        if [ -s "$cc_storage_pid_file" ]; then
            cc_storage_pid=$(cat "$cc_storage_pid_file")
            case "$cc_storage_pid" in
                ''|*[!0-9]*)
                    echo "cc-storage: invalid helper PID" >&2
                    return 1
                    ;;
            esac
            cc_storage_target="/proc/$cc_storage_pid/root/mnt/storage"
            if [ -f "$cc_storage_target/.ready" ]; then
                break
            fi
        fi
        sleep 1
        cc_storage_waited=$((cc_storage_waited + 1))
    done
    if [ "$cc_storage_waited" -ge "$cc_storage_max_wait" ]; then
        echo "cc-storage: timed out waiting for the LUKS mount" >&2
        return 1
    fi

    if [ -L "$DATA_DIR" ]; then
        if [ "$(readlink "$DATA_DIR")" = "$cc_storage_target" ]; then
            return 0
        fi
        echo "cc-storage: $DATA_DIR points to a different mount" >&2
        return 1
    fi
    # Only replace an empty directory. An existing mount or model stays intact.
    if [ -d "$DATA_DIR" ]; then
        rmdir "$DATA_DIR" || return 1
    fi
    if [ -e "$DATA_DIR" ]; then
        echo "cc-storage: $DATA_DIR already exists" >&2
        return 1
    fi
    ln -s "$cc_storage_target" "$DATA_DIR" || return 1
    echo "cc-storage: $DATA_DIR -> $cc_storage_target"
}

cc_storage_link || exit 1
