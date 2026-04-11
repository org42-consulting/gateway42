FROM python:3.13-slim

WORKDIR /gateway

# Install dependencies first (cached layer)
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy application code
COPY app/ .

# Create directories for persistent data (overridden by volumes in production)
RUN mkdir -p db logs

EXPOSE 7000

CMD ["hypercorn", "routes:app", "--bind", "0.0.0.0:7000", "--workers", "2"]
