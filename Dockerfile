# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy only go.mod first
COPY go.mod ./

# Download dependencies and create go.sum
RUN go mod download && \
    go mod verify

# Copy source code
COPY . .

# Build the plugin
RUN CGO_ENABLED=0 GOOS=linux go build -o /connectionbalancer

# Final stage
FROM alpine:latest

WORKDIR /

# Copy the binary from builder
COPY --from=builder /connectionbalancer /connectionbalancer

# Copy Traefik configuration
COPY .traefik.yml /traefik.yml

# Expose port that will be used
EXPOSE 80

# Set the entry point
ENTRYPOINT ["/connectionbalancer"]