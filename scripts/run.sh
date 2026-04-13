#!/bin/bash

set -e  # Exit on any error

echo
echo ">>> Starting Gateway42."
echo

# Check if Python is available
if ! command -v python3 &> /dev/null; then
    echo "Error: Python 3 is not installed or not in PATH"
    exit 1
fi

# Create necessary directories if they don't exist
mkdir -p db
mkdir -p logs

# Set default environment variables if not set
export GW42_SECRET_KEY="${GW42_SECRET_KEY:-change-this-in-production}"
export GW42_DB_PATH="${GW42_DB_PATH:-./db/gateway.db}"
export OLLAMA_URL="${OLLAMA_URL:-http://127.0.0.1:11434/api/chat}"
export ADMIN_PASSWORD="${ADMIN_PASSWORD:-admin123}"
export DEFAULT_RATE_LIMIT="${DEFAULT_RATE_LIMIT:-10}"
export SESSION_TIMEOUT="${SESSION_TIMEOUT:-3600}"


echo "Starting gateway on port 7000. Access the admin interface at http://localhost:7000/"
python3 app/app.py

echo "Gateway stopped."
echo
