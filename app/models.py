import sqlite3
import logging
import threading
from datetime import datetime, timezone
from config import DB_PATH
from contextlib import contextmanager
from utils import cached_user_lookup, invalidate_user_cache

logger = logging.getLogger(__name__)

# Connection pool
_db_pool: list = []
_db_pool_lock = threading.Lock()


def get_db() -> sqlite3.Connection:
    with _db_pool_lock:
        if _db_pool:
            conn = _db_pool.pop()
        else:
            conn = sqlite3.connect(DB_PATH, timeout=30, check_same_thread=False)
    conn.row_factory = sqlite3.Row
    return conn


def return_db(conn: sqlite3.Connection) -> None:
    with _db_pool_lock:
        if len(_db_pool) < 10:
            _db_pool.append(conn)
        else:
            conn.close()


@contextmanager
def db_transaction():
    conn = get_db()
    try:
        yield conn
        conn.commit()
    except Exception:
        conn.rollback()
        raise
    finally:
        return_db(conn)


def init_db() -> None:
    from werkzeug.security import generate_password_hash
    from config import ADMIN_EMAIL, ADMIN_PASSWORD

    with db_transaction() as conn:
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("""
            CREATE TABLE IF NOT EXISTS users(
                id         INTEGER PRIMARY KEY AUTOINCREMENT,
                email      TEXT UNIQUE NOT NULL,
                api_key    TEXT UNIQUE NOT NULL,
                status     TEXT NOT NULL DEFAULT 'pending',
                rate_limit INTEGER NOT NULL DEFAULT 10,
                created_at TEXT NOT NULL
            )
        """)
        conn.execute("""
            CREATE TABLE IF NOT EXISTS admin_user(
                id            INTEGER PRIMARY KEY AUTOINCREMENT,
                email         TEXT UNIQUE NOT NULL,
                password_hash TEXT NOT NULL,
                created_at    TEXT NOT NULL
            )
        """)
        conn.execute("""
            CREATE TABLE IF NOT EXISTS logs(
                id       INTEGER PRIMARY KEY AUTOINCREMENT,
                user_id  INTEGER NOT NULL,
                model    TEXT,
                prompt   TEXT,
                response TEXT,
                ts       TEXT NOT NULL
            )
        """)
        # Migration: add model column to existing databases
        existing = {row[1] for row in conn.execute("PRAGMA table_info(logs)")}
        if "model" not in existing:
            conn.execute("ALTER TABLE logs ADD COLUMN model TEXT")
        conn.execute("""
            CREATE TABLE IF NOT EXISTS settings(
                key   TEXT PRIMARY KEY,
                value TEXT NOT NULL
            )
        """)
        conn.execute("CREATE INDEX IF NOT EXISTS idx_logs_user_id ON logs(user_id)")
        conn.execute(
            "CREATE UNIQUE INDEX IF NOT EXISTS idx_users_api_key ON users(api_key)"
        )

        if not conn.execute(
            "SELECT 1 FROM admin_user WHERE email=?", (ADMIN_EMAIL,)
        ).fetchone():
            conn.execute(
                "INSERT INTO admin_user(email, password_hash, created_at) VALUES(?,?,?)",
                (
                    ADMIN_EMAIL,
                    generate_password_hash(ADMIN_PASSWORD, method="pbkdf2:sha256"),
                    datetime.now(timezone.utc).isoformat(),
                ),
            )
        logger.info("Database initialized")

    from utils import init_rate_limiter_table
    init_rate_limiter_table()


# ── User queries ──────────────────────────────────────────────────────────────

@cached_user_lookup
def get_user_by_id(user_id: int):
    conn = get_db()
    try:
        return conn.execute("SELECT * FROM users WHERE id=?", (user_id,)).fetchone()
    finally:
        return_db(conn)


def get_user_by_email(email: str):
    conn = get_db()
    try:
        return conn.execute("SELECT * FROM users WHERE email=?", (email,)).fetchone()
    finally:
        return_db(conn)


def get_user_by_api_key(api_key: str):
    conn = get_db()
    try:
        return conn.execute(
            "SELECT * FROM users WHERE api_key=?", (api_key,)
        ).fetchone()
    finally:
        return_db(conn)


def get_all_users():
    conn = get_db()
    try:
        return conn.execute(
            "SELECT * FROM users ORDER BY created_at DESC"
        ).fetchall()
    finally:
        return_db(conn)


def create_user(email: str, api_key: str, rate_limit: int = 10, status: str = "disabled") -> None:
    conn = get_db()
    try:
        conn.execute(
            "INSERT INTO users(email, api_key, status, rate_limit, created_at) "
            "VALUES(?,?,?,?,?)",
            (email, api_key, status, rate_limit, datetime.now(timezone.utc).isoformat()),
        )
        conn.commit()
        logger.info("User %s created", email)
    except sqlite3.IntegrityError:
        raise ValueError("User already exists")
    finally:
        return_db(conn)


def update_user_status(user_id: int, status: str) -> None:
    conn = get_db()
    try:
        conn.execute("UPDATE users SET status=? WHERE id=?", (status, user_id))
        conn.commit()
    finally:
        return_db(conn)
    invalidate_user_cache(user_id)


def reset_user_api_key(user_id: int, new_api_key: str) -> None:
    conn = get_db()
    try:
        conn.execute(
            "UPDATE users SET api_key=? WHERE id=?", (new_api_key, user_id)
        )
        conn.commit()
    finally:
        return_db(conn)
    invalidate_user_cache(user_id)


def update_user_rate_limit(user_id: int, rate_limit: int) -> None:
    conn = get_db()
    try:
        conn.execute("UPDATE users SET rate_limit=? WHERE id=?", (rate_limit, user_id))
        conn.commit()
    finally:
        return_db(conn)
    invalidate_user_cache(user_id)


def reset_system() -> None:
    """Delete all audit logs and rate-limit entries, leaving users and settings intact."""
    conn = get_db()
    try:
        conn.execute("DELETE FROM logs")
        conn.execute("DELETE FROM rate_limit_entries")
        conn.commit()
    finally:
        return_db(conn)


def delete_user(user_id: int) -> None:
    conn = get_db()
    try:
        conn.execute("DELETE FROM logs WHERE user_id=?", (user_id,))
        conn.execute("DELETE FROM users WHERE id=?", (user_id,))
        conn.commit()
    finally:
        return_db(conn)
    invalidate_user_cache(user_id)


def log_interaction(user_id: int, prompt: str, response: str, model: str = "") -> None:
    conn = get_db()
    try:
        conn.execute(
            "INSERT INTO logs(user_id, model, prompt, response, ts) VALUES(?,?,?,?,?)",
            (user_id, model, prompt, response, datetime.now(timezone.utc).isoformat()),
        )
        conn.commit()
    finally:
        return_db(conn)


def get_user_logs(user_id: int):
    conn = get_db()
    try:
        return conn.execute(
            "SELECT id, model, prompt, response, ts FROM logs "
            "WHERE user_id=? ORDER BY ts ASC",
            (user_id,),
        ).fetchall()
    finally:
        return_db(conn)


def get_logs(search: str = "", limit: int = 200):
    conn = get_db()
    try:
        if search:
            pattern = f"%{search}%"
            return conn.execute(
                "SELECT l.id, u.email, l.model, l.prompt, l.response, l.ts "
                "FROM logs l JOIN users u ON u.id = l.user_id "
                "WHERE l.prompt LIKE ? OR l.response LIKE ? OR u.email LIKE ? "
                "ORDER BY l.ts DESC LIMIT ?",
                (pattern, pattern, pattern, limit),
            ).fetchall()
        return conn.execute(
            "SELECT l.id, u.email, l.model, l.prompt, l.response, l.ts "
            "FROM logs l JOIN users u ON u.id = l.user_id "
            "ORDER BY l.ts DESC LIMIT ?",
            (limit,),
        ).fetchall()
    finally:
        return_db(conn)


# ── Admin queries ─────────────────────────────────────────────────────────────

def get_admin_by_email(email: str):
    conn = get_db()
    try:
        return conn.execute(
            "SELECT * FROM admin_user WHERE email=?", (email,)
        ).fetchone()
    finally:
        return_db(conn)


def update_admin_password(admin_id: int, password_hash: str) -> None:
    conn = get_db()
    try:
        conn.execute(
            "UPDATE admin_user SET password_hash=? WHERE id=?",
            (password_hash, admin_id),
        )
        conn.commit()
    finally:
        return_db(conn)


# ── Settings ──────────────────────────────────────────────────────────────────

def get_setting(key: str, default: str | None = None) -> str | None:
    conn = get_db()
    try:
        row = conn.execute(
            "SELECT value FROM settings WHERE key=?", (key,)
        ).fetchone()
        return row["value"] if row else default
    finally:
        return_db(conn)


def set_setting(key: str, value: str) -> None:
    conn = get_db()
    try:
        conn.execute(
            "INSERT INTO settings(key, value) VALUES(?,?) "
            "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (key, value),
        )
        conn.commit()
    finally:
        return_db(conn)
