#!/bin/bash -e
# entrypoint for bifrost-aigw service

FILENAME="$(basename $0)"

[[ "${DEBUG:-0}" =~ ^(1|y|Y) ]] && set -x

log_info() {
    echo -e "$(date '+%Y-%m-%dT%H:%M:%S.0%z')\tINFO\t${FILENAME}\t$*"
}

log_error() {
    echo -e "$(date '+%Y-%m-%dT%H:%M:%S.0%z')\tERROR\t${FILENAME}\t$*" 1>&2
}

prep_term()
{
    unset bifrost_pid
    unset term_kill_needed
    trap 'handle_term' TERM INT
}

handle_term()
{
    if [ "${bifrost_pid}" ]; then
        kill -TERM "${bifrost_pid}" 2>/dev/null
    else
        term_kill_needed="yes"
    fi
}

wait_term()
{
    local return_val=0
    sleep 1
    bifrost_pid=$(pgrep -n bifrost-aigw)
    if [ "${term_kill_needed}" ]; then
        kill -TERM "${bifrost_pid}" 2>/dev/null
    fi
    wait ${bifrost_pid} || return_val=$?
    trap - TERM INT
    wait ${bifrost_pid} || return_val=$?
    return ${return_val}
}

# Ensure data directory exists with correct permissions
if [ -d "/app/data" ]; then
    mkdir -p /app/data/logs
fi

# Default values
APP_PORT="${APP_PORT:-8080}"
APP_HOST="${APP_HOST:-0.0.0.0}"
APP_DIR="${APP_DIR:-/app/data}"
LOG_LEVEL="${LOG_LEVEL:-info}"
LOG_STYLE="${LOG_STYLE:-json}"

log_info "Starting bifrost-aigw on ${APP_HOST}:${APP_PORT}"

prep_term

exec /bin/bifrost-aigw \
    -host "${APP_HOST}" \
    -port "${APP_PORT}" \
    -app-dir "${APP_DIR}" \
    -log-level "${LOG_LEVEL}" \
    -log-style "${LOG_STYLE}" \
    "$@"
