import asyncio
import collections
import csv
import io
import json
import logging
import urllib.parse

import httpx
from quart import (
    Quart, flash, jsonify, redirect, render_template,
    request, Response, session, url_for,
)

import sys
import os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from config import ADMIN_EMAIL, OLLAMA_URL, SECRET_KEY, SESSION_TIMEOUT, validate_config
from openai_compat import (
    format_stream_chunk, new_completion_id, ollama_tags_to_openai_models,
    ollama_to_openai, openai_error, openai_to_ollama,
)
from models import (
    create_user, delete_user, get_admin_by_email, get_all_users,
    get_setting, get_user_by_api_key, get_user_by_id, init_db,
    log_interaction, reset_system, reset_user_api_key, set_setting,
    update_admin_password, update_user_status, get_user_logs,
    update_user_rate_limit, get_logs,
)
from utils import (
    generate_api_key, hash_password, rate_limiter,
    truncate_input, validate_email, validate_password, verify_password,
)

logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s',
)
logger = logging.getLogger(__name__)


class _SystemLogHandler(logging.Handler):
    """Keeps the last MAX_ENTRIES log records in memory for the admin UI."""
    MAX_ENTRIES = 500

    def __init__(self):
        super().__init__()
        self._buf: collections.deque = collections.deque(maxlen=self.MAX_ENTRIES)

    def emit(self, record: logging.LogRecord) -> None:
        from datetime import datetime, timezone
        self._buf.append({
            "ts":    datetime.fromtimestamp(record.created, tz=timezone.utc).strftime("%Y-%m-%dT%H:%M:%S"),
            "level": record.levelname,
            "name":  record.name,
            "msg":   record.getMessage(),
        })

    def entries(self) -> list:
        return list(self._buf)


_syslog_handler = _SystemLogHandler()
logging.getLogger().addHandler(_syslog_handler)

app = Quart(__name__, static_folder=os.path.dirname(os.path.abspath(__file__)), static_url_path="")
app.secret_key = SECRET_KEY
app.permanent_session_lifetime = SESSION_TIMEOUT


# ── CORS ───────────────────────────────────────────────────────────────────────

_CORS_HEADERS = {
    "Access-Control-Allow-Origin":  "*",
    "Access-Control-Allow-Methods": "GET, POST, OPTIONS",
    "Access-Control-Allow-Headers": "Authorization, Content-Type",
    "Access-Control-Max-Age":       "86400",
}


@app.route("/v1/<path:path>", methods=["OPTIONS"])
async def cors_preflight(**_):
    """Handle CORS preflight for all /v1/* endpoints."""
    return Response(status=204, headers=_CORS_HEADERS)


@app.after_request
async def add_cors_headers(response):
    """Attach CORS headers to every /v1/* response."""
    if request.path.startswith("/v1/"):
        response.headers.update(_CORS_HEADERS)
    return response


# ── Startup ────────────────────────────────────────────────────────────────────

@app.before_serving
async def startup():
    validate_config()
    await asyncio.to_thread(init_db)
    app.add_background_task(_rate_limit_cleanup_loop)


async def _rate_limit_cleanup_loop():
    """Periodically purge expired rate-limit entries from the DB."""
    while True:
        await asyncio.sleep(300)
        try:
            await asyncio.to_thread(rate_limiter.cleanup_old_entries)
        except Exception as e:
            logger.error("Rate limiter cleanup error: %s", e)


# ── Helpers ────────────────────────────────────────────────────────────────────

async def _get_ollama_base_url() -> str:
    """Return the Ollama base URL (scheme://host:port), preferring admin-saved settings."""
    saved_url = await asyncio.to_thread(get_setting, "ollama_url", None)
    if saved_url:
        saved_port = await asyncio.to_thread(get_setting, "ollama_port", "11434")
        return f"{saved_url}:{saved_port}"
    parsed = urllib.parse.urlparse(OLLAMA_URL)
    scheme = parsed.scheme or "http"
    host   = parsed.hostname or "127.0.0.1"
    port   = parsed.port or 11434
    return f"{scheme}://{host}:{port}"


# ── UI ─────────────────────────────────────────────────────────────────────────

@app.route("/")
async def index():
    return await render_template("login.html")


@app.route("/logout")
async def logout():
    session.clear()
    logger.info("Admin logged out")
    return redirect(url_for("index"))


@app.route("/register", methods=["POST"])
async def register():
    if not session.get("admin"):
        return redirect(url_for("index"))
    try:
        f = await request.form
        email = truncate_input(f.get("email", ""))

        if not validate_email(email):
            await flash("Invalid email format", "error")
            return redirect(url_for("admin_panel"))

        api_key = generate_api_key()
        await asyncio.to_thread(create_user, email, api_key)
        logger.info("User %s registered", email)

        return (
            f"<h2 style='font-family:sans-serif'>Registration Successful</h2>"
            f"<p>API key for <strong>{email}</strong>:</p>"
            f"<pre style='background:#111;color:#0f0;padding:12px'>{api_key}</pre>"
            f"<p>Save this key — it will not be shown again.</p>"
            f"<p><a href='{url_for('admin_panel')}'>Back to admin panel</a></p>"
        )
    except ValueError as e:
        await flash(str(e), "error")
        return redirect(url_for("admin_panel"))
    except Exception as e:
        logger.error("Registration error: %s", e)
        return "Internal server error", 500


# ── Admin ──────────────────────────────────────────────────────────────────────

@app.route("/admin", methods=["POST"])
async def admin_login():
    try:
        f = await request.form
        password = f.get("password", "")

        admin = await asyncio.to_thread(get_admin_by_email, ADMIN_EMAIL)
        if not admin or not verify_password(admin["password_hash"], password):
            await flash("Invalid password", "error")
            return redirect(url_for("index"))

        session["admin"] = True
        session.permanent = True
        logger.info("Admin successfully logged in")
        return redirect(url_for("admin_panel"))
    except Exception as e:
        logger.error("Admin login error: %s", e)
        return "Internal server error", 500


@app.route("/admin/panel")
async def admin_panel():
    if not session.get("admin"):
        return redirect(url_for("index"))
    try:
        users = [dict(u) for u in await asyncio.to_thread(get_all_users)]

        # Resolve current Ollama settings (DB overrides env)
        saved_url = await asyncio.to_thread(get_setting, "ollama_url", None)
        saved_port = await asyncio.to_thread(get_setting, "ollama_port", None)
        if saved_url:
            ollama_url = saved_url
            ollama_port = int(saved_port or "11434")
        else:
            parsed = urllib.parse.urlparse(OLLAMA_URL)
            ollama_url = f"http://{parsed.hostname or '127.0.0.1'}"
            ollama_port = parsed.port or 11434

        # Check whether Ollama is reachable and fetch models
        ollama_models = []
        try:
            async with httpx.AsyncClient(timeout=3) as client:
                r = await client.get(f"{ollama_url}:{ollama_port}/api/version")
                ollama_status = r.status_code == 200
            if ollama_status:
                async with httpx.AsyncClient(timeout=5) as client:
                    r = await client.get(f"{ollama_url}:{ollama_port}/api/tags")
                    if r.status_code == 200:
                        ollama_models = [m["name"] for m in r.json().get("models", [])]
        except Exception:
            ollama_status = False

        return await render_template(
            "dashboard.html",
            users=users,
            ollama_url=ollama_url,
            ollama_port=ollama_port,
            ollama_status=ollama_status,
            ollama_models=ollama_models,
        )
    except Exception as e:
        logger.error("Admin panel error: %s", e)
        return "Internal server error", 500


@app.route("/admin/settings-page")
async def admin_ollama_settings_page():
    if not session.get("admin"):
        return redirect(url_for("index"))
    try:
        saved_url = await asyncio.to_thread(get_setting, "ollama_url", None)
        saved_port = await asyncio.to_thread(get_setting, "ollama_port", None)
        if saved_url:
            ollama_url = saved_url
            ollama_port = int(saved_port or "11434")
        else:
            parsed = urllib.parse.urlparse(OLLAMA_URL)
            ollama_url = f"http://{parsed.hostname or '127.0.0.1'}"
            ollama_port = parsed.port or 11434

        ollama_models = []
        ollama_model_details = []
        try:
            async with httpx.AsyncClient(timeout=3) as client:
                r = await client.get(f"{ollama_url}:{ollama_port}/api/version")
                ollama_status = r.status_code == 200
            if ollama_status:
                async with httpx.AsyncClient(timeout=5) as client:
                    r = await client.get(f"{ollama_url}:{ollama_port}/api/tags")
                    if r.status_code == 200:
                        for m in r.json().get("models", []):
                            ollama_models.append(m["name"])
                            size_b = m.get("size", 0)
                            if size_b >= 1_000_000_000:
                                size_str = f"{size_b / 1_000_000_000:.1f} GB"
                            else:
                                size_str = f"{size_b / 1_000_000:.0f} MB"
                            ollama_model_details.append({"name": m["name"], "size": size_str})
        except Exception:
            ollama_status = False

        return await render_template(
            "settings.html",
            ollama_url=ollama_url,
            ollama_port=ollama_port,
            ollama_status=ollama_status,
            ollama_models=ollama_models,
            ollama_model_details=ollama_model_details,
        )
    except Exception as e:
        logger.error("Ollama settings page error: %s", e)
        return "Internal server error", 500


@app.route("/admin/ollama-test")
async def admin_ollama_test():
    if not session.get("admin"):
        return redirect(url_for("index"))
    base_url = await _get_ollama_base_url()
    try:
        async with httpx.AsyncClient(timeout=5) as client:
            r = await client.get(f"{base_url}/api/version")
            if r.status_code == 200:
                version = r.json().get("version", "unknown")
                await flash(f"Connected — Ollama version {version}", "success")
            else:
                await flash(f"Ollama responded with status {r.status_code}", "error")
    except Exception as e:
        await flash(f"Could not reach Ollama: {e}", "error")
    return redirect(url_for("admin_ollama_settings_page"))


@app.route("/admin/ollama-pull-stream")
async def ollama_pull_stream():
    if not session.get("admin"):
        return jsonify({"error": "Unauthorized"}), 401
    model = request.args.get("model", "").strip()
    if not model:
        return jsonify({"error": "Model name required"}), 400

    base_url = await _get_ollama_base_url()

    async def generate():
        try:
            async with httpx.AsyncClient(timeout=600) as client:
                async with client.stream(
                    "POST", f"{base_url}/api/pull", json={"name": model, "stream": True}
                ) as resp:
                    if resp.status_code != 200:
                        yield f"data: {json.dumps({'error': f'Ollama returned {resp.status_code}'})}\n\n"
                        return
                    async for line in resp.aiter_lines():
                        if line.strip():
                            yield f"data: {line}\n\n"
        except Exception as e:
            yield f"data: {json.dumps({'error': str(e)})}\n\n"
        yield "data: [DONE]\n\n"

    return Response(
        generate(),
        content_type="text/event-stream",
        headers={"X-Accel-Buffering": "no", "Cache-Control": "no-cache"},
    )


@app.route("/admin/ollama-delete-model", methods=["POST"])
async def ollama_delete_model():
    if not session.get("admin"):
        return redirect(url_for("index"))
    f = await request.form
    model = f.get("model", "").strip()
    if not model:
        await flash("Model name is required", "error")
        return redirect(url_for("admin_ollama_settings_page"))
    base_url = await _get_ollama_base_url()
    try:
        async with httpx.AsyncClient(timeout=30) as client:
            r = await client.request("DELETE", f"{base_url}/api/delete", json={"name": model})
            if r.status_code in (200, 404):
                await flash(f"Model '{model}' deleted.", "success")
            else:
                await flash(f"Ollama returned {r.status_code}", "error")
    except Exception as e:
        await flash(f"Could not delete model: {e}", "error")
    return redirect(url_for("admin_ollama_settings_page"))


@app.route("/admin/ollama-settings", methods=["POST"])
async def admin_ollama_settings():
    if not session.get("admin"):
        return redirect(url_for("index"))
    try:
        f = await request.form
        ollama_url = f.get("ollama_url", "").strip()
        port = f.get("port", "").strip()

        if not ollama_url or not port:
            await flash("URL and port are required", "error")
            return redirect(url_for("admin_ollama_settings_page"))

        try:
            port_int = int(port)
            if not (1 <= port_int <= 65535):
                raise ValueError()
        except ValueError:
            await flash("Port must be a number between 1 and 65535", "error")
            return redirect(url_for("admin_ollama_settings_page"))

        await asyncio.to_thread(set_setting, "ollama_url", ollama_url)
        await asyncio.to_thread(set_setting, "ollama_port", port)
        logger.info("Ollama settings updated: %s:%s", ollama_url, port)
        await flash("Ollama settings saved", "success")
        return redirect(url_for("admin_ollama_settings_page"))
    except Exception as e:
        logger.error("Ollama settings error: %s", e)
        return "Internal server error", 500


@app.route("/admin/change-password", methods=["POST"])
async def admin_change_password():
    if not session.get("admin"):
        return redirect(url_for("index"))
    try:
        f = await request.form
        current = f.get("current", "")
        new = f.get("new", "")
        confirm = f.get("confirm", "")

        admin = await asyncio.to_thread(get_admin_by_email, ADMIN_EMAIL)
        if not admin or not verify_password(admin["password_hash"], current):
            await flash("Invalid current password", "error")
            return redirect(url_for("admin_ollama_settings_page"))

        if new != confirm:
            await flash("Passwords do not match", "error")
            return redirect(url_for("admin_ollama_settings_page"))

        valid, msg = validate_password(new)
        if not valid:
            await flash(msg, "error")
            return redirect(url_for("admin_ollama_settings_page"))

        await asyncio.to_thread(update_admin_password, admin["id"], hash_password(new))
        logger.info("Admin password changed")
        await flash("Password updated successfully", "success")
        return redirect(url_for("admin_ollama_settings_page"))
    except Exception as e:
        logger.error("Admin password change error: %s", e)
        return "Internal server error", 500


@app.route("/toggle/<int:uid>", methods=["POST"])
async def toggle(uid):
    if not session.get("admin"):
        return redirect(url_for("index"))
    try:
        user = await asyncio.to_thread(get_user_by_id, uid)
        if not user:
            return "User not found", 404
        new_status = "active" if user["status"] != "active" else "disabled"
        await asyncio.to_thread(update_user_status, uid, new_status)
        logger.info("User %d status → %s", uid, new_status)
        return redirect(url_for("admin_panel"))
    except Exception as e:
        logger.error("Toggle user error: %s", e)
        return "Internal server error", 500


@app.route("/admin/reset/<int:uid>", methods=["POST"])
async def admin_reset(uid):
    if not session.get("admin"):
        return redirect(url_for("index"))
    try:
        user = await asyncio.to_thread(get_user_by_id, uid)
        if not user:
            return "User not found", 404
        new_api_key = generate_api_key()
        await asyncio.to_thread(reset_user_api_key, uid, new_api_key)
        logger.info("API key reset for user %d (%s)", uid, user["email"])
        await flash(f"New API key for {user['email']}: {new_api_key}", "success")
        return redirect(url_for("admin_panel"))
    except Exception as e:
        logger.error("Admin reset error: %s", e)
        return "Internal server error", 500


@app.route("/admin/update-rate-limit/<int:uid>", methods=["POST"])
async def admin_update_rate_limit(uid):
    if not session.get("admin"):
        return redirect(url_for("index"))
    try:
        f = await request.form
        rate_limit = int(f.get("rate_limit", 10))
        if rate_limit < 1:
            rate_limit = 1
        if rate_limit > 1000:
            rate_limit = 1000

        await asyncio.to_thread(update_user_rate_limit, uid, rate_limit)
        logger.info("Rate limit updated for user %d: %d", uid, rate_limit)
        await flash(f"Rate limit updated for user {uid}: {rate_limit} requests/minute", "success")
        return redirect(url_for("admin_panel"))
    except Exception as e:
        logger.error("Admin update rate limit error: %s", e)
        return "Internal server error", 500


# ── Logs ──────────────────────────────────────────────────────────────────────

@app.route("/admin/help")
async def admin_help():
    if not session.get("admin"):
        return redirect(url_for("index"))
    return await render_template("help.html")


@app.route("/admin/logs")
async def admin_logs():
    if not session.get("admin"):
        return redirect(url_for("index"))
    try:
        search = request.args.get("q", "").strip()
        rows = [dict(r) for r in await asyncio.to_thread(get_logs, search)]
        return await render_template("logs.html", logs=rows, search=search)
    except Exception as e:
        logger.error("Logs page error: %s", e)
        return "Internal server error", 500


@app.route("/admin/logs/data")
async def admin_logs_data():
    """JSON endpoint used by the auto-refresh poller on the logs page."""
    if not session.get("admin"):
        return jsonify({"error": "Unauthorized"}), 401
    try:
        search = request.args.get("q", "").strip()
        page   = max(1, int(request.args.get("page", 1)))
        rows   = [dict(r) for r in await asyncio.to_thread(get_logs, search, 10_000)]
        total  = len(rows)
        pages  = max(1, (total + 9) // 10)
        page   = min(page, pages)
        rows   = rows[(page - 1) * 10 : page * 10]
        return jsonify({"logs": rows, "count": len(rows), "total": total,
                        "page": page, "pages": pages, "search": search})
    except Exception as e:
        logger.error("Logs data error: %s", e)
        return jsonify({"error": "Internal server error"}), 500


@app.route("/admin/logs/system")
async def admin_system_logs():
    """JSON endpoint: returns in-memory system log entries for the admin UI."""
    if not session.get("admin"):
        return jsonify({"error": "Unauthorized"}), 401
    q      = request.args.get("q", "").strip().lower()
    page   = max(1, int(request.args.get("page", 1)))
    entries = _syslog_handler.entries()
    entries.reverse()  # newest first
    if q:
        entries = [e for e in entries if q in e["msg"].lower()]
    total  = len(entries)
    pages  = max(1, (total + 9) // 10)
    page   = min(page, pages)
    entries = entries[(page - 1) * 10 : page * 10]
    return jsonify({"logs": entries, "count": len(entries), "total": total,
                    "page": page, "pages": pages, "search": q})


@app.route("/admin/logs/system/export")
async def admin_system_logs_export():
    """Download in-memory system log entries as CSV."""
    if not session.get("admin"):
        return redirect(url_for("index"))
    q = request.args.get("q", "").strip().lower()
    entries = _syslog_handler.entries()
    entries.reverse()
    if q:
        entries = [e for e in entries if q in e["msg"].lower()]
    buf = io.StringIO()
    writer = csv.writer(buf)
    writer.writerow(["timestamp", "level", "logger", "message"])
    for e in entries:
        writer.writerow([e["ts"], e["level"], e["name"], e["msg"]])
    return Response(
        buf.getvalue(),
        mimetype="text/csv",
        headers={
            "Content-Disposition": "attachment; filename=system_logs.csv",
            "Cache-Control": "no-store",
        },
    )


@app.route("/admin/logs/system/reset", methods=["POST"])
async def admin_system_logs_reset():
    """Clear the in-memory system log buffer."""
    if not session.get("admin"):
        return jsonify({"error": "Unauthorized"}), 401
    _syslog_handler._buf.clear()
    logger.info("System log buffer cleared by admin")
    return jsonify({"ok": True})


# ── CSV Export ─────────────────────────────────────────────────────────────────

@app.route("/export/<int:uid>")
async def export(uid):
    if not session.get("admin"):
        return redirect(url_for("index"))
    try:
        rows = await asyncio.to_thread(get_user_logs, uid)

        buf = io.StringIO()
        writer = csv.writer(buf)
        writer.writerow(["log_id", "prompt", "response", "timestamp"])
        for r in rows:
            writer.writerow([r["id"], r["prompt"], r["response"], r["ts"]])

        session[f"exported_{uid}"] = True
        logger.info("CSV log export for user %d", uid)
        return Response(
            buf.getvalue(),
            mimetype="text/csv",
            headers={
                "Content-Disposition": f"attachment; filename=user_{uid}_audit.csv",
                "Cache-Control": "no-store",
            },
        )
    except Exception as e:
        logger.error("CSV export error: %s", e)
        return "Internal server error", 500


@app.route("/admin/export-logs")
async def export_all_logs():
    if not session.get("admin"):
        return redirect(url_for("index"))
    try:
        rows = await asyncio.to_thread(get_logs, "", 10_000_000)

        buf = io.StringIO()
        writer = csv.writer(buf)
        writer.writerow(["log_id", "email", "model", "prompt", "response", "timestamp"])
        for r in rows:
            writer.writerow([r["id"], r["email"], r["model"], r["prompt"], r["response"], r["ts"]])
        logger.info("CSV export of all logs")
        return Response(
            buf.getvalue(),
            mimetype="text/csv",
            headers={
                "Content-Disposition": "attachment; filename=all_logs.csv",
                "Cache-Control": "no-store",
            },
        )
    except Exception as e:
        logger.error("Export all logs error: %s", e)
        return "Internal server error", 500


@app.route("/confirm-delete/<int:uid>")
async def confirm_delete(uid):
    if not session.get("admin"):
        return redirect(url_for("index"))
    return await render_template("confirm_delete.html", uid=uid)


@app.route("/delete/<int:uid>", methods=["POST"])
async def delete(uid):
    if not session.get("admin"):
        return redirect(url_for("index"))
    if not session.get(f"exported_{uid}"):
        return "Export required before delete", 403
    try:
        await asyncio.to_thread(delete_user, uid)
        session.pop(f"exported_{uid}", None)
        logger.info("User %d deleted", uid)
        return redirect(url_for("admin_panel"))
    except Exception as e:
        logger.error("Delete user error: %s", e)
        return "Internal server error", 500


@app.route("/admin/reset-system", methods=["POST"])
async def admin_reset_system():
    if not session.get("admin"):
        return redirect(url_for("index"))
    try:
        await asyncio.to_thread(reset_system)
        logger.info("System reset: all logs and rate-limit entries cleared")
        await flash("System reset: all logs and rate-limit entries have been cleared.", "success")
        next_page = (await request.form).get("next", "admin_panel")
        if next_page not in {"admin_panel", "admin_logs"}:
            next_page = "admin_panel"
        return redirect(url_for(next_page))
    except Exception as e:
        logger.error("System reset error: %s", e)
        return "Internal server error", 500


# ── API ────────────────────────────────────────────────────────────────────────

@app.route("/health")
async def health():
    logger.debug("Health check passed")
    return {"status": "ok"}


async def _get_authenticated_user():
    """Validate Bearer token and return the active user row, or None on failure."""
    auth_header = request.headers.get("Authorization", "")
    if not auth_header.startswith("Bearer "):
        return None
    api_key = auth_header[7:]
    user = await asyncio.to_thread(get_user_by_api_key, api_key)
    if not user or user["status"] != "active":
        return None
    return user


def _sanitize_messages(messages: list) -> list:
    return [
        {"role": msg.get("role", ""), "content": truncate_input(msg.get("content", ""))}
        for msg in messages
    ]


@app.route("/v1/models")
async def list_models():
    """OpenAI-compatible GET /v1/models — proxies Ollama's /api/tags."""
    user = await _get_authenticated_user()
    if user is None:
        return jsonify(openai_error("Invalid API key", "authentication_error")), 401

    base_url = await _get_ollama_base_url()
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            resp = await client.get(f"{base_url}/api/tags")
            resp.raise_for_status()
            return jsonify(ollama_tags_to_openai_models(resp.json()))
    except httpx.RequestError as e:
        logger.error("Ollama /api/tags error: %s", e)
        return jsonify(openai_error("Could not reach Ollama", "api_error")), 502


@app.route("/v1/chat/completions", methods=["POST"])
async def chat_completions():
    """
    OpenAI-compatible POST /v1/chat/completions.

    Handles both streaming (stream=true) and non-streaming requests.
    Auth: Authorization: Bearer <api_key>
    """
    user = await _get_authenticated_user()
    if user is None:
        return jsonify(openai_error("Invalid API key", "authentication_error")), 401

    if not await asyncio.to_thread(rate_limiter.is_allowed, user["id"], user["rate_limit"]):
        return jsonify(openai_error("Rate limit exceeded", "rate_limit_error")), 429

    data = await request.get_json()
    if not data or "messages" not in data:
        return jsonify(openai_error("'messages' is required")), 400

    messages  = _sanitize_messages(data["messages"])
    ollama_req = openai_to_ollama(data, messages)
    base_url   = await _get_ollama_base_url()
    chat_url   = f"{base_url}/api/chat"

    if data.get("stream", False):
        return Response(
            _stream_completions(chat_url, ollama_req, user["id"], messages, ollama_req.get("model", "")),
            content_type="text/event-stream",
            headers={"X-Accel-Buffering": "no", "Cache-Control": "no-cache"},
        )


    # ── Non-streaming ─────────────────────────────────────────────────────────

    try:
        async with httpx.AsyncClient(timeout=120) as client:
            resp = await client.post(chat_url, json=ollama_req)
            resp.raise_for_status()
            result = ollama_to_openai(resp.json())
    except httpx.RequestError as e:
        logger.error("Ollama request error: %s", e)
        return jsonify(openai_error("Could not reach Ollama", "api_error")), 502
    except httpx.HTTPStatusError as e:
        logger.error("Ollama HTTP error %s: %s", e.response.status_code, e)
        return jsonify(openai_error("Upstream error", "api_error")), 502

    await asyncio.to_thread(log_interaction, user["id"], str(messages), str(result), ollama_req.get("model", ""))
    return jsonify(result)


async def _stream_completions(chat_url: str, ollama_req: dict, user_id: int, messages: list, model: str = ""):
    """Async generator that streams OpenAI-format SSE chunks from Ollama."""
    completion_id = new_completion_id()
    is_first = True

    try:
        async with httpx.AsyncClient(timeout=120) as client:
            async with client.stream("POST", chat_url, json=ollama_req) as resp:
                resp.raise_for_status()
                async for line in resp.aiter_lines():
                    if not line.strip():
                        continue
                    try:
                        chunk = json.loads(line)
                    except json.JSONDecodeError:
                        continue

                    openai_chunk = format_stream_chunk(chunk, completion_id, is_first)
                    is_first = False
                    yield f"data: {json.dumps(openai_chunk)}\n\n"

                    if chunk.get("done"):
                        break
    except Exception as e:
        logger.error("Stream error: %s", e)
        yield f"data: {json.dumps(openai_error('Internal error', 'api_error'))}\n\n"
        return

    yield "data: [DONE]\n\n"
    await asyncio.to_thread(log_interaction, user_id, str(messages), "streamed", model)


@app.route("/users")
async def users_panel():
    if not session.get("admin"):
        return redirect(url_for("index"))
    return redirect(url_for("admin_panel"))
