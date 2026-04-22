# HTTPS Setup for Gateway42

This guide configures trusted HTTPS for gateway42 using two components:

- **acme.sh** — obtains and auto-renews a free Let's Encrypt certificate using Cloudflare DNS. No firewall ports need to be open during certificate issuance.
- **Caddy** — a lightweight reverse proxy that handles TLS and forwards requests to gateway42.

Gateway42 itself keeps running on HTTP port 7000 (localhost only, unchanged). Caddy sits in front and handles all TLS.

```
Client → HTTPS :8443 → Caddy (TLS termination) → HTTP :7000 → gateway42 → Ollama :11434
```

> **Why port 8443 and not 443?** macOS does not allow non-root processes to bind to ports below 1024. Port 8443 is the standard HTTPS alternative and requires no special permissions. If you need the standard port 443, see [Using Port 443](#using-port-443-optional) at the end of this guide.

---

## Prerequisites

- gateway42 is already installed and running (see [README_SERVICE.md](README_SERVICE.md))
- You have a domain managed in Cloudflare (this guide uses `gateway.org42.net` as an example — substitute your own)
- Homebrew is installed (`/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"`)

---

## Part 1 — Get a Certificate with acme.sh

acme.sh proves you own the domain by temporarily adding a DNS TXT record via the Cloudflare API. Let's Encrypt checks for the record, issues the certificate, and acme.sh removes the record. No inbound ports are required at any point.

### Step 1.1 — Install acme.sh

```bash
curl https://get.acme.sh | sh -s email=your@email.com
```

Replace `your@email.com` with your real address — Let's Encrypt will send certificate expiry warnings there.

The installer places acme.sh in `~/.acme.sh/` and adds a daily cron job for automatic renewal. Close and reopen your terminal to load the new shell environment, or run:

```bash
source ~/.acme.sh/acme.sh.env
```

Confirm the installation:

```bash
acme.sh --version
```

---

### Step 1.2 — Create a Cloudflare API Token

You need a token that allows acme.sh to add and remove DNS records on your domain.

1. Log in to the [Cloudflare dashboard](https://dash.cloudflare.com)
2. Click your profile avatar (top right corner) → **My Profile**
3. Select **API Tokens** from the left sidebar
4. Click **Create Token**
5. Find the **"Edit zone DNS"** template and click **Use template** next to it
6. Under **Zone Resources**, configure the three dropdowns:
   - First: `Include`
   - Second: `Specific zone`
   - Third: your domain (e.g. `org42.net`)
7. Leave everything else at defaults
8. Click **Continue to summary** → **Create Token**
9. **Copy the token immediately** — it is only shown once. Store it somewhere safe.

Test that the token works:

```bash
curl -s -X GET "https://api.cloudflare.com/client/v4/user/tokens/verify" \
     -H "Authorization: Bearer YOUR_TOKEN_HERE" \
     -H "Content-Type: application/json" | python3 -m json.tool
```

You should see `"status": "active"` in the response. If you see `"status": "expired"` or an error, the token was not created correctly — go back and create a new one.

---

### Step 1.3 — Create the Certificate Directory

```bash
mkdir -p ~/.gateway42/tls
chmod 700 ~/.gateway42/tls
```

The `chmod 700` ensures the private key is only readable by your user.

---

### Step 1.4 — Issue the Certificate

Run the following command. Replace `gateway.org42.net` with your actual subdomain and `YOUR_TOKEN_HERE` with the Cloudflare token from Step 1.2.

```bash
export CF_Token="YOUR_TOKEN_HERE"

acme.sh --issue \
  --dns dns_cf \
  -d gateway.org42.net \
  --cert-file      ~/.gateway42/tls/cert.pem \
  --key-file       ~/.gateway42/tls/key.pem \
  --fullchain-file ~/.gateway42/tls/fullchain.pem \
  --reloadcmd      "brew services restart caddy"
```

What each flag does:

| Flag | Purpose |
|------|---------|
| `--dns dns_cf` | Use Cloudflare DNS for the ACME challenge |
| `-d gateway.org42.net` | The domain to issue the cert for |
| `--cert-file` | Where to write the certificate |
| `--key-file` | Where to write the private key |
| `--fullchain-file` | Certificate + intermediate chain (what Caddy needs) |
| `--reloadcmd` | Command to run after every successful renewal |

The process takes 1–2 minutes. You will see acme.sh add the DNS record, wait for verification, then write the certificate files. It ends with `Cert success`.

If you see an error about DNS propagation, wait 60 seconds and retry.

---

### Step 1.5 — Store the Token for Auto-renewal

The cron job that renews your certificate needs access to the Cloudflare token. Save it to acme.sh's config file so it is available automatically:

```bash
echo "CF_Token='YOUR_TOKEN_HERE'" >> ~/.acme.sh/account.conf
```

Verify it was saved:

```bash
grep CF_Token ~/.acme.sh/account.conf
```

---

### Step 1.6 — Verify the Certificate

```bash
acme.sh --list
```

You should see `gateway.org42.net` listed with a `SAN Domains` column and an expiry date roughly 90 days from now.

Also check the files exist:

```bash
ls -lh ~/.gateway42/tls/
# cert.pem, key.pem, fullchain.pem — all non-zero size
```

---

## Part 2 — Configure Caddy

Caddy listens on port 8443, presents the certificate obtained by acme.sh, and proxies all requests to gateway42 on port 7000.

### Step 2.1 — Install Caddy

```bash
brew install caddy
```

---

### Step 2.2 — Find Your Caddyfile Location

The location depends on your Mac's chip:

```bash
# Apple Silicon (M1/M2/M3)
echo /opt/homebrew/etc/Caddyfile

# Intel
echo /usr/local/etc/Caddyfile
```

If you are unsure which you have, run `uname -m`. Output `arm64` means Apple Silicon; `x86_64` means Intel.

---

### Step 2.3 — Write the Caddyfile

Open the Caddyfile for editing (use the path from Step 2.2):

```bash
# Apple Silicon:
nano /opt/homebrew/etc/Caddyfile

# Intel:
nano /usr/local/etc/Caddyfile
```

Replace the entire contents with:

```
# Disable Caddy's built-in automatic HTTPS — we supply our own certificate
{
    auto_https off
}

gateway.org42.net:8443 {
    # Use the certificate obtained by acme.sh
    tls /Users/org42/.gateway42/tls/fullchain.pem /Users/org42/.gateway42/tls/key.pem

    # Forward all requests to gateway42
    reverse_proxy localhost:7000
}
```

**Important:** Replace `gateway.org42.net` with your actual subdomain, and `/Users/org42/` with your actual home directory. To get your home directory path:

```bash
echo $HOME
```

Save and exit (`Ctrl+O`, `Enter`, `Ctrl+X` in nano).

---

### Step 2.4 — Validate the Configuration

Before starting Caddy, check the config for syntax errors:

```bash
# Apple Silicon:
caddy validate --config /opt/homebrew/etc/Caddyfile

# Intel:
caddy validate --config /usr/local/etc/Caddyfile
```

You should see `Valid configuration`. If there are errors, re-check the file for typos, especially the certificate paths.

---

### Step 2.5 — Start Caddy as a Service

```bash
brew services start caddy
```

Check it is running:

```bash
brew services list | grep caddy
# caddy   started   org42   ~/Library/LaunchAgents/homebrew.mxcl.caddy.plist
```

The `started` status means Caddy is running. If it shows `error`, check the logs:

```bash
tail -50 $(brew --prefix)/var/log/caddy/caddy.log 2>/dev/null \
  || log show --predicate 'process == "caddy"' --last 5m
```

---

### Step 2.6 — Verify End-to-End

Test the health endpoint through HTTPS:

```bash
curl https://gateway.org42.net:8443/health
# Expected: {"status":"ok"}
```

Inspect the certificate to confirm it is from Let's Encrypt and not self-signed:

```bash
echo | openssl s_client -connect gateway.org42.net:8443 2>/dev/null \
  | openssl x509 -noout -issuer -subject -dates
```

Expected output (dates will vary):

```
subject=CN=gateway.org42.net
issuer=C=US, O=Let's Encrypt, CN=R11
notBefore=Apr 16 12:00:00 2026 GMT
notAfter=Jul 15 12:00:00 2026 GMT
```

If the issuer shows `Let's Encrypt`, your HTTPS setup is complete.

---

## Auto-renewal

acme.sh installed a daily cron job during setup. Certificates are renewed automatically when they are within 30 days of expiry (roughly every 60 days). After renewal, acme.sh runs `brew services restart caddy` to load the new certificate.

Confirm the cron job is in place:

```bash
crontab -l | grep acme
```

You should see a line like:

```
0 0 * * * ~/.acme.sh/acme.sh --cron --home ~/.acme.sh ...
```

To test that renewal and the Caddy reload work correctly, do a forced dry run:

```bash
acme.sh --renew -d gateway.org42.net --force
```

It should end with `Reload success` after running the reload command.

---

## Using Port 443 (Optional)

If you want clients to connect to `https://gateway.org42.net` without specifying a port, you need to run Caddy on port 443. macOS does not allow this for regular user processes, but you can redirect traffic from port 443 to 8443 using the built-in packet filter.

### Create the redirect rule

```bash
echo "rdr pass inet proto tcp from any to any port 443 -> 127.0.0.1 port 8443" \
  | sudo tee /etc/pf.anchors/gateway42
```

### Activate it

```bash
sudo pfctl -ef /etc/pf.anchors/gateway42
```

### Make it persist across reboots

```bash
sudo tee /Library/LaunchDaemons/com.gateway42.pf.plist > /dev/null << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.gateway42.pf</string>
    <key>ProgramArguments</key>
    <array>
        <string>/sbin/pfctl</string>
        <string>-ef</string>
        <string>/etc/pf.anchors/gateway42</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
</dict>
</plist>
EOF

sudo launchctl bootstrap system /Library/LaunchDaemons/com.gateway42.pf.plist
```

After this, clients can use `https://gateway.org42.net` with no port number.

---

## Troubleshooting

**`curl` returns a connection refused error:**
```bash
# Is gateway42 running on port 7000?
curl http://localhost:7000/health

# Is Caddy running?
brew services list | grep caddy
```

**Caddy starts but returns a certificate error in the browser:**
```bash
# Check Caddy is using the right cert files
caddy validate --config /opt/homebrew/etc/Caddyfile

# Check the cert files exist and are non-empty
ls -lh ~/.gateway42/tls/
```

**acme.sh fails with "DNS not yet propagated":**

DNS changes can take a few minutes to propagate. Wait 2–3 minutes and retry:
```bash
acme.sh --issue --dns dns_cf -d gateway.org42.net --force
```

**acme.sh renewal fails in cron (Cloudflare token error):**
```bash
# Confirm the token is saved
grep CF_Token ~/.acme.sh/account.conf

# Test the token is still valid (tokens can be revoked in Cloudflare)
curl -s "https://api.cloudflare.com/client/v4/user/tokens/verify" \
     -H "Authorization: Bearer $(grep CF_Token ~/.acme.sh/account.conf | cut -d"'" -f2)"
```

**Check certificate expiry at any time:**
```bash
openssl x509 -in ~/.gateway42/tls/cert.pem -noout -dates
```
