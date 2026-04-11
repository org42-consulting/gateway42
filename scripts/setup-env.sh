set -e pipefail
echo

if [ -z "$VENV" ]; then
    VENV="./venv"
fi

# Check if the virtual environment directory exists
if [ -d "$VENV" ]; then
    echo "[!] Virtual environment already exists at $VENV"
else
    echo "[✓] Creating Python virtual environment"
    python3 -m venv "$VENV"
    echo "Done."
fi

# Activate the virtual environment and install dependencies
echo;echo "[✓] Activating Python environment"
source ${VENV}/bin/activate
${VENV}/bin/pip install --upgrade pip
echo "Done."

echo;echo "[✓] Installing Python dependencies"
${VENV}/bin/pip install -r requirements.txt
echo "Done."

echo;echo "[✓] Python environment setup complete"
echo