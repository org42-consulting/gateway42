#!/usr/bin/env bash
set -e
sudo launchctl stop com.ollama.gateway 2>/dev/null || true
sudo launchctl unload /Library/LaunchDaemons/com.ollama.gateway.plist 2>/dev/null || true
sudo rm -f /Library/LaunchDaemons/com.ollama.gateway.plist
sudo rm -rf /opt/ollama-gateway
echo "[✓] Ollama Gateway fully removed"

