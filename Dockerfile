FROM golang:1.22-alpine AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /app/server .

# ── Runtime image ──────────────────────────────────────────────────────────────
FROM alpine:latest
WORKDIR /gateway

COPY --from=builder /app/server .

RUN mkdir -p db logs

EXPOSE 7000

CMD ["./server"]
