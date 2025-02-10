# Traefik Connection Balancer Plugin

A Traefik middleware plugin that balances WebSocket connections across multiple backend pods based on their current connection load. This plugin is specifically designed to distribute WebSocket connections evenly while considering the existing connection count on each backend pod.

## Features

- Load balancing based on active connection counts
- WebSocket connection support with bidirectional relay
- Automatic failover when pods are unavailable
- Configurable metric path for connection counting
- Support for both HTTP and WebSocket protocols
- TLS/SSL support for secure WebSocket connections

## Prerequisites

- Go 1.22 or higher
- Traefik v2.10.4 or higher
- Docker (for building and running the container)
- Make (optional, for using Makefile commands)

## Installation

1. Clone the repository:
```bash
git clone https://gitlab.com/ktrust/devops/solution/traefikwsbalancer.git
cd traefikwsbalancer
```

2. Build the plugin:
```bash
make all
```

Or manually:
```bash
go mod tidy
go mod vendor
go build -o connectionbalancer
```

## Configuration

### Plugin Configuration

Add the following to your Traefik static configuration (traefik.yml):

```yaml
experimental:
  plugins:
    connectionbalancer:
      moduleName: "gitlab.com/ktrust/devops/solution/traefikwsbalancer"
      version: "v2.10.4"
```

### Dynamic Configuration

Configure the plugin in your dynamic configuration:

```yaml
http:
  middlewares:
    connectionbalancer:
      plugin:
        connectionbalancer:
          metricPath: "/metric"
          pods:
            - "http://kbrain-socket-agent-ktrust-service.phaedra.svc.cluster.local:80"
            - "http://kbrain-user-socket-ktrust-service.phaedra.svc.cluster.local:80"
```

## Development

### Directory Structure

```
.
├── .gitlab-ci.yml             # GitLab CI/CD configuration
├── .golangci.yml              # Golangci-lint configuration
├── .traefik.yml               # Traefik configuration
├── Dockerfile                 # Multi-stage build configuration
├── Makefile                   # Build automation
├── connectionbalancer.go      # Main plugin implementation
├── connectionbalancer_test.go # Unit tests
├── go.mod                     # Go module definition
└── go.sum                     # Go module checksums
```

### Available Make Commands

- `make all`: Run all build steps (tidy, vendor, build)
- `make tidy`: Run go mod tidy
- `make vendor`: Create vendor directory
- `make lint`: Run golangci-lint
- `make test`: Run unit tests
- `make build`: Build the plugin binary

### Running Tests

```bash
make test
```

Or manually:
```bash
go test -v ./...
```

### Linting

```bash
make lint
```

## CI/CD Pipeline

The project includes a GitLab CI/CD pipeline with the following stages:

- `lint`: Runs golangci-lint for code quality checks
- `test`: Executes unit tests
- `build`: Builds and pushes multi-arch Docker images

The pipeline automatically builds and pushes Docker images for both AMD64 and ARM64 architectures when:
- A new tag is pushed
- Changes are merged to the main branch (manual trigger required)

## Docker

### Building the Image

```bash
docker build -t traefikwsbalancer .
```

### Running the Container

```bash
docker run -p 80:80 traefikwsbalancer
```

## Contributing

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add some amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Merge Request

## License

[Include your license information here]

## Contact

[Include contact information or maintainer details here]