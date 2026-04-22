#!/bin/bash

# Service management script for gateway42

SERVICE_NAME="com.gateway42.service"
SERVICE_PLIST="$HOME/Library/LaunchAgents/gateway42.plist"

case "$1" in
    start)
        echo "Starting gateway42 service..."
        launchctl load "$SERVICE_PLIST"
        ;;
    stop)
        echo "Stopping gateway42 service..."
        launchctl unload "$SERVICE_PLIST"
        ;;
    restart)
        echo "Restarting gateway42 service..."
        launchctl unload "$SERVICE_PLIST"
        sleep 2
        launchctl load "$SERVICE_PLIST"
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