#!/bin/bash

# Service management script for gateway42

SERVICE_NAME="com.gateway42.service"
# Must match the filename install.sh writes, which is derived from the Label
# above, not from the repo's gateway42.plist filename.
SERVICE_PLIST="$HOME/Library/LaunchAgents/${SERVICE_NAME}.plist"

DOMAIN="gui/$(id -u)"

require_plist() {
    if [ ! -f "$SERVICE_PLIST" ]; then
        echo "No LaunchAgent found at $SERVICE_PLIST" >&2
        echo "Run ./install.sh first — it generates the plist." >&2
        exit 1
    fi
}

case "$1" in
    start)
        require_plist
        echo "Starting gateway42 service..."
        launchctl bootstrap "$DOMAIN" "$SERVICE_PLIST"
        ;;
    stop)
        echo "Stopping gateway42 service..."
        launchctl bootout "$DOMAIN/$SERVICE_NAME"
        ;;
    restart)
        require_plist
        echo "Restarting gateway42 service..."
        launchctl bootout "$DOMAIN/$SERVICE_NAME" 2>/dev/null
        sleep 1
        launchctl bootstrap "$DOMAIN" "$SERVICE_PLIST"
        ;;
    status)
        echo "Checking gateway42 service status..."
        launchctl list | grep "$SERVICE_NAME"
        ;;
    *)
        echo "Usage: $0 {start|stop|restart|status}"
        exit 1
        ;;
esac

exit 0