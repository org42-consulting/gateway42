import re
import logging
import time
import threading
import functools
from werkzeug.security import generate_password_hash, check_password_hash
from config import MAX_MESSAGE_LENGTH

logger = logging.getLogger(__name__)

# Simple in-memory cache for user lookups
_user_cache: dict = {}
_cache_lock = threading.Lock()
_CACHE_TTL = 300  # 5 minutes


def cached_user_lookup(func):
    """Decorator that caches user lookups keyed by function name + positional args."""
    @functools.wraps(func)
    def wrapper(*args, **kwargs):
        cache_key = (func.__name__,) + args + tuple(sorted(kwargs.items()))
        with _cache_lock:
            if cache_key in _user_cache:
                cached_data, timestamp = _user_cache[cache_key]
                if time.time() - timestamp < _CACHE_TTL:
                    return cached_data
        result = func(*args, **kwargs)
        with _cache_lock:
            _user_cache[cache_key] = (result, time.time())
        return result
    return wrapper


def invalidate_user_cache(user_id: int) -> None:
    """Remove all cache entries for a given user_id."""
    with _cache_lock:
        stale = [k for k in _user_cache if len(k) >= 2 and k[1] == user_id]
        for k in stale:
            del _user_cache[k]


def validate_email(email: str) -> bool:
    pattern = r'^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$'
    return re.match(pattern, email) is not None


def validate_password(password: str) -> tuple[bool, str]:
    if len(password) < 8:
        return False, "Password must be at least 8 characters"
    if not re.search(r'[A-Z]', password):
        return False, "Password must contain at least one uppercase letter"
    if not re.search(r'[a-z]', password):
        return False, "Password must contain at least one lowercase letter"
    if not re.search(r'\d', password):
        return False, "Password must contain at least one digit"
    return True, ""


def hash_password(password: str) -> str:
    return generate_password_hash(password, method="pbkdf2:sha256")


def verify_password(password_hash: str, password: str) -> bool:
    """Verify a plaintext password against its stored hash."""
    return check_password_hash(password_hash, password)


def generate_api_key(length: int = 20) -> str:
    """Generate a URL-safe random API key.

    Args:
        length: Token byte length, must be between 16 and 32.

    Raises:
        ValueError: If length is outside the valid range.
    """
    if not (16 <= length <= 32):
        raise ValueError(f"length must be between 16 and 32, got {length}")
    import secrets
    return secrets.token_urlsafe(length)


def truncate_input(text: str, max_length: int = MAX_MESSAGE_LENGTH) -> str:
    """Truncate text to at most max_length characters."""
    if not text:
        return ""
    return str(text)[:max_length]


class DatabaseRateLimiter:
    """Database-backed rate limiter, safe for multi-process deployments."""

    def __init__(self):
        self.window = 60  # seconds

    def is_allowed(self, user_id: int, limit: int) -> bool:
        from models import get_db, return_db
        conn = get_db()
        try:
            cutoff = time.time() - self.window
            conn.execute(
                "DELETE FROM rate_limit_entries WHERE timestamp < ?", (cutoff,)
            )
            count = conn.execute(
                "SELECT COUNT(*) FROM rate_limit_entries WHERE user_id = ?",
                (user_id,),
            ).fetchone()[0]
            if count >= limit:
                conn.commit()
                return False
            conn.execute(
                "INSERT INTO rate_limit_entries (user_id, timestamp) VALUES (?, ?)",
                (user_id, time.time()),
            )
            conn.commit()
            return True
        finally:
            return_db(conn)

    def cleanup_old_entries(self) -> None:
        from models import get_db, return_db
        conn = get_db()
        try:
            conn.execute(
                "DELETE FROM rate_limit_entries WHERE timestamp < ?",
                (time.time() - self.window,),
            )
            conn.commit()
        except Exception as e:
            logger.error("Rate limiter cleanup error: %s", e)
        finally:
            return_db(conn)


def init_rate_limiter_table() -> None:
    from models import get_db, return_db
    conn = get_db()
    try:
        conn.execute("""
            CREATE TABLE IF NOT EXISTS rate_limit_entries(
                id        INTEGER PRIMARY KEY AUTOINCREMENT,
                user_id   INTEGER NOT NULL,
                timestamp REAL NOT NULL
            )
        """)
        conn.execute(
            "CREATE INDEX IF NOT EXISTS idx_rate_limit_user_timestamp "
            "ON rate_limit_entries(user_id, timestamp)"
        )
        conn.commit()
    except Exception as e:
        logger.error("Rate limiter table init error: %s", e)
    finally:
        return_db(conn)


rate_limiter = DatabaseRateLimiter()
