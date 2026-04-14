FROM golang:1.26-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /api ./cmd/api
RUN CGO_ENABLED=0 GOOS=linux go build -o /worker ./cmd/worker

# --- API image ---
FROM gcr.io/distroless/static AS api
COPY --from=builder /app/config /config
COPY --from=builder /api /api
ENTRYPOINT ["/api"]

# --- Worker image ---
FROM gcr.io/distroless/static AS worker
COPY --from=builder /app/config /config
COPY --from=builder /worker /worker
ENTRYPOINT ["/worker"]
