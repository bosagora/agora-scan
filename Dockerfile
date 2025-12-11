# Build stage
FROM golang:1.18 AS build-env

# Install build dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    make \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN make -B all

# Final stage
FROM ubuntu:22.04

RUN apt-get update && apt-get -y upgrade && apt-get install -y --no-install-recommends \
    libssl-dev \
    ca-certificates \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy built binaries and necessary files
COPY --from=build-env /src/bin /app/
COPY --from=build-env /src/config /app/config

# Make explorer executable
RUN chmod +x /app/explorer

# Expose default port (adjust if needed based on your config)
EXPOSE 3333

# Run the explorer
CMD ["./explorer", "--config", "./config/default.config.yml"]