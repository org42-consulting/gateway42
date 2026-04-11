import os
from routes import app

if __name__ == "__main__":
    debug = os.getenv("DEBUG", "false").lower() == "true"
    app.run(host="0.0.0.0", port=7000, debug=debug)

