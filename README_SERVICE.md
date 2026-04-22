# macOS Service Setup for Gateway42

Gateway42 can run as a persistent macOS LaunchAgent that starts automatically at login and restarts on crash.

## Install

```bash
./install.sh
```

That's it. The script handles everything:

- Builds the binary if needed (`go build`)
- Installs it to `/usr/local/bin/gateway42`
- Creates data directories: `~/.gateway42/db/` and `~/Library/Logs/gateway42/`
- Writes a LaunchAgent plist to `~/Library/LaunchAgents/com.gateway42.service.plist`
- Loads and starts the service

After install, gateway42 is available at **http://localhost:7000**.

## Service Management

```bash
# Check status (PID and last exit code)
launchctl list com.gateway42.service

# Follow live logs
tail -f ~/Library/Logs/gateway42/gateway.log

# Stop the service
launchctl stop com.gateway42.service

# Start the service
launchctl start com.gateway42.service

# Restart
launchctl stop com.gateway42.service && launchctl start com.gateway42.service
```

## Uninstall

```bash
./install.sh uninstall
```

Removes the binary (`/usr/local/bin/gateway42`) and the plist. Data is preserved at `~/.gateway42/` and `~/Library/Logs/gateway42/`.

To also remove data:

```bash
rm -rf ~/.gateway42 ~/Library/Logs/gateway42
```

## Service Details

| Item | Path |
| ---- | ---- |
| Binary | `/usr/local/bin/gateway42` |
| Plist | `~/Library/LaunchAgents/com.gateway42.service.plist` |
| Database | `~/.gateway42/db/gateway.db` |
| Logs | `~/Library/Logs/gateway42/gateway.log` |
| Port | `7000` (HTTP) |

The service is configured with `KeepAlive: SuccessfulExit = false` — it restarts automatically on crash but stops cleanly when you use `launchctl stop`.

## Configuration

Environment variables are set inside the plist under `EnvironmentVariables`. Edit the plist to change them:

```bash
nano ~/Library/LaunchAgents/com.gateway42.service.plist
```

Then reload:

```bash
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.gateway42.service.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.gateway42.service.plist
```

See the main [README.md](README.md#configuration) for all available variables, including `ADMIN_PASSWORD`, `TLS_CERT`/`TLS_KEY` for HTTPS, and others.

## HTTPS

To expose gateway42 over HTTPS with a trusted Let's Encrypt certificate, see the dedicated guide:

**[HTTPS_SETUP.md](HTTPS_SETUP.md)**

It covers certificate issuance via Cloudflare DNS (no open ports required), Caddy as a TLS reverse proxy, auto-renewal, and optional port 443 setup.

---

## Troubleshooting

**Service fails to load:**
```bash
plutil -lint ~/Library/LaunchAgents/com.gateway42.service.plist
```

**Binary blocked by Gatekeeper (extended attribute `@` on binary):**
```bash
xattr -d com.apple.quarantine /usr/local/bin/gateway42
```
`install.sh` does this automatically, but if you copied the binary manually you may need to run it yourself.

**View raw launchd exit reason:**
```bash
launchctl list com.gateway42.service
# "LastExitStatus" non-zero = crashed or failed to start; check the log file
```
