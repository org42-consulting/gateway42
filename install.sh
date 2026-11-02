#!/bin/bash
# install.sh — Install gateway42 as a macOS LaunchAgent service
# Usage:
#   ./install.sh           — install / reinstall
#   ./install.sh uninstall — stop and remove the service

set -euo pipefail

LABEL="com.gateway42.service"
BINARY_NAME="gateway42"
INSTALL_DIR="/usr/local/bin"
BINARY_SRC="$(cd "$(dirname "$0")" && pwd)/${BINARY_NAME}"
BINARY_DEST="${INSTALL_DIR}/${BINARY_NAME}"
DATA_DIR="${HOME}/.gateway42"
DB_DIR="${DATA_DIR}/db"
LOG_DIR="${HOME}/Library/Logs/gateway42"
PLIST_DIR="${HOME}/Library/LaunchAgents"
PLIST_PATH="${PLIST_DIR}/${LABEL}.plist"
DOMAIN="gui/$(id -u)"

# ── helpers ────────────────────────────────────────────────────────────────────
info()  { echo "  [info]  $*"; }
ok()    { echo "  [ok]    $*"; }
err()   { echo "  [error] $*" >&2; exit 1; }
warn()  { echo "  [warn]  $*"; }

service_loaded() {
    launchctl print "${DOMAIN}/${LABEL}" &>/dev/null
}

stop_service() {
    launchctl bootout "${DOMAIN}/${LABEL}" 2>/dev/null || true
    # bootout returns before launchd finishes tearing the job down; a
    # bootstrap issued too early fails with "5: Input/output error".
    for _ in {1..20}; do
        service_loaded || return 0
        sleep 0.5
    done
    warn "Service still registered after 10s; bootstrap may fail"
}

# ── uninstall ─────────────────────────────────────────────────────────────────
if [[ "${1:-}" == "uninstall" ]]; then
    echo "Uninstalling gateway42 service..."
    if service_loaded; then
        info "Stopping and unloading service..."
        stop_service
        ok "Service stopped"
    else
        warn "Service was not loaded"
    fi
    [[ -f "${PLIST_PATH}" ]]   && rm "${PLIST_PATH}"   && ok "Removed ${PLIST_PATH}"
    [[ -f "${BINARY_DEST}" ]]  && rm "${BINARY_DEST}"  && ok "Removed ${BINARY_DEST}"
    echo ""
    echo "Uninstall complete. Data directory preserved at: ${DATA_DIR}"
    echo "To also remove data:  rm -rf ${DATA_DIR} ${LOG_DIR}"
    exit 0
fi

# ── install ───────────────────────────────────────────────────────────────────
echo "Installing gateway42..."
echo ""

# 1. Build binary if it doesn't exist
if [[ ! -f "${BINARY_SRC}" ]]; then
    info "Binary not found at ${BINARY_SRC} — building..."
    if ! command -v go &>/dev/null; then
        err "Go is not installed. Install Go or build the binary manually first."
    fi
    (cd "$(dirname "$0")" && go build -o "${BINARY_NAME}" .) \
        || err "Build failed. Check Go errors above."
    ok "Build succeeded"
else
    ok "Binary found at ${BINARY_SRC}"
fi

# 2. Copy binary to /usr/local/bin (may require sudo)
if [[ -w "${INSTALL_DIR}" ]]; then
    cp "${BINARY_SRC}" "${BINARY_DEST}"
else
    info "Installing to ${INSTALL_DIR} requires sudo..."
    sudo cp "${BINARY_SRC}" "${BINARY_DEST}"
    sudo chmod 755 "${BINARY_DEST}"
fi

# Remove quarantine attribute if present (set by macOS on downloaded/copied binaries)
xattr -d com.apple.quarantine "${BINARY_DEST}" 2>/dev/null || true
ok "Binary installed at ${BINARY_DEST}"

# 3. Create data directories
mkdir -p "${DB_DIR}" "${LOG_DIR}" "${PLIST_DIR}"
ok "Data directories ready"
info "  DB    → ${DB_DIR}/gateway.db"
info "  Logs  → ${LOG_DIR}/"

# 4. Stop existing service if running
if service_loaded; then
    info "Stopping existing service..."
    stop_service
fi

# 5. Write plist (absolute paths — launchd does not expand ~ or $HOME)
cat > "${PLIST_PATH}" << PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${LABEL}</string>

    <key>ProgramArguments</key>
    <array>
        <string>${BINARY_DEST}</string>
    </array>

    <key>EnvironmentVariables</key>
    <dict>
        <key>GW42_DB_PATH</key>
        <string>${DB_DIR}/gateway.db</string>
        <key>LOG_FILE</key>
        <string>${LOG_DIR}/gateway.log</string>
        <key>PORT</key>
        <string>7000</string>
        <key>LOG_LEVEL</key>
        <string>INFO</string>
    </dict>

    <key>WorkingDirectory</key>
    <string>${DATA_DIR}</string>

    <key>StandardOutPath</key>
    <string>${LOG_DIR}/gateway.log</string>

    <key>StandardErrorPath</key>
    <string>${LOG_DIR}/gateway.log</string>

    <key>RunAtLoad</key>
    <true/>

    <!-- Restart automatically if the process exits unexpectedly -->
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>

    <!-- Throttle rapid restart loops (seconds) -->
    <key>ThrottleInterval</key>
    <integer>10</integer>
</dict>
</plist>
PLIST

ok "Plist written to ${PLIST_PATH}"

# 6. Load the service
launchctl bootstrap "${DOMAIN}" "${PLIST_PATH}" \
    || err "Failed to load service. Check the plist with: plutil -lint ${PLIST_PATH}"

ok "Service loaded"
echo ""
echo "────────────────────────────────────────"
echo " gateway42 is running on port 7000"
echo " Admin panel: http://localhost:7000"
echo ""
echo " Manage the service:"
echo "   Status:  launchctl print ${DOMAIN}/${LABEL}"
echo "   Logs:    tail -f ${LOG_DIR}/gateway.log"
echo "   Restart: launchctl kickstart -k ${DOMAIN}/${LABEL}"
echo "   Stop:    launchctl bootout ${DOMAIN}/${LABEL}"
echo "   Start:   launchctl bootstrap ${DOMAIN} ${PLIST_PATH}"
echo "   Remove:  ./install.sh uninstall"
echo "────────────────────────────────────────"
