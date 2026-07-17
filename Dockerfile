# Build stage
FROM golang:1.25.12-bookworm AS builder

RUN apt-get update && apt-get install -y gcc libsqlite3-dev && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -ldflags="-s -w" -o /only-fan-controller ./cmd/controller

# Runtime stage - slim Debian base. This service only shells out to nvidia-smi;
# it doesn't link against CUDA/driver libraries directly. The NVIDIA Container
# Toolkit's "utility" driver capability (the default when NVIDIA_DRIVER_CAPABILITIES
# is unset, per NVIDIA's docs: required for nvidia-smi/NVML) injects the nvidia-smi
# binary and driver libraries into the container at `docker run` time via
# --gpus all/--runtime=nvidia, regardless of base image - a CUDA base image is not
# required. This drops the runtime image from ~"nvidia/cuda:12.4.1-runtime" (multiple
# GB) to a slim Debian base (tens of MB).
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y \
    ipmitool \
    libsqlite3-0 \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENV NVIDIA_VISIBLE_DEVICES=all
ENV NVIDIA_DRIVER_CAPABILITIES=utility

WORKDIR /app

COPY --from=builder /only-fan-controller /usr/local/bin/only-fan-controller
COPY config.example.yaml /etc/only-fan-controller/config.yaml

RUN mkdir -p /var/lib/only-fan-controller /var/log

EXPOSE 8086

ENTRYPOINT ["/usr/local/bin/only-fan-controller"]
CMD ["--config", "/etc/only-fan-controller/config.yaml"]
