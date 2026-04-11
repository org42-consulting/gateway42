import os
import sys

SECRET_KEY = os.getenv('OLLAMA_GATEWAY_SECRET_KEY', '')
DB_PATH = os.getenv('OLLAMA_GATEWAY_DB_PATH', './db/gateway.db')
OLLAMA_URL = os.getenv('OLLAMA_URL', 'http://127.0.0.1:11434/api/chat')
ADMIN_EMAIL = os.getenv('ADMIN_EMAIL', 'admin')
ADMIN_PASSWORD = os.getenv('ADMIN_PASSWORD', '')
DEFAULT_RATE_LIMIT = int(os.getenv('DEFAULT_RATE_LIMIT', '10'))
SESSION_TIMEOUT = int(os.getenv('SESSION_TIMEOUT', '3600'))  # seconds

# Logging
LOG_LEVEL = os.getenv('LOG_LEVEL', 'INFO')
LOG_FILE = os.getenv('LOG_FILE', './logs/gateway.log')

# Other
MAX_MESSAGE_LENGTH = int(os.getenv('MAX_MESSAGE_LENGTH', '10000'))


def validate_config() -> None:
    """Fail fast at startup if required environment variables are missing."""
    errors = []
    if not SECRET_KEY:
        errors.append("OLLAMA_GATEWAY_SECRET_KEY must be set")
    if not ADMIN_PASSWORD:
        errors.append("ADMIN_PASSWORD must be set")
    if errors:
        for err in errors:
            print(f"CONFIG ERROR: {err}", file=sys.stderr)
        sys.exit(1)
