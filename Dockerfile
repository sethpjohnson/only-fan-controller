# Build stage
FROM golang:1.22-bookworm AS builder

RUN apt-get update && apt-get install -y gcc libsqlite3-dev && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -ldflags="-s -w" -o /smart-fan-controller ./cmd/controller

# Runtime stage - NVIDIA CUDA base for built-in nvidia-smi support
FROM nvidia/cuda:12.4.1-runtime-ubuntu22.04

RUN apt-get update && apt-get install -y \
    ipmitool \
    libsqlite3-0 \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /smart-fan-controller /usr/local/bin/smart-fan-controller
COPY config.example.yaml /etc/smart-fan-controller/config.yaml

RUN mkdir -p /var/lib/smart-fan-controller /var/log

EXPOSE 8086

ENTRYPOINT ["/usr/local/bin/smart-fan-controller"]
CMD ["--config", "/etc/smart-fan-controller/config.yaml"]
