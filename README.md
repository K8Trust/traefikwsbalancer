# Traefik WebSocket Connection Balancer

A high-performance WebSocket connection balancer middleware for Traefik that distributes WebSocket connections across multiple backend pods based on their active connection count. This plugin is designed to ensure optimal load distribution for WebSocket-heavy applications.

## Features

- **Intelligent Load Balancing**
  - Distributes connections based on real-time connection counts
  - Automatic selection of least-loaded backend
  - Connection count caching with configurable TTL
  - Graceful failover on backend failures

- **WebSocket Support**
  - Full WebSocket protocol support (RFC 6455)
  - Bidirectional message relay
  - Transparent protocol upgrade handling
  - Support for WebSocket extensions and subprotocols

- **Advanced Configuration**
  - Configurable metrics endpoint
  - Adjustable timeouts (dial, read, write)
  - TLS/SSL verification options
  - Custom header forwarding

- **Monitoring & Debugging**
  - Detailed logging of connection events
  - Connection metrics exposure
  - Error tracking and reporting
  - Debug-friendly log messages

## Quick Start

### Prerequisites
- Go 1.22 or higher
- gorilla/websocket v1.5.3 or higher

### Installation

1. Clone the repository:
```bash
git clone https://github.com/yourusername/traefikwsbalancer.git
cd traefikwsbalancer
```

2. Install dependencies:
```bash
go mod tidy
```

3. Build the project:
```bash
go build ./...
```

### Basic Usage

1. Start the test server (provides backend WebSocket endpoints):
```bash
go run cmd/test-server/main.go
```

2. In a separate terminal, run the test client:
```bash
go run cmd/test-client/main.go
```

## Configuration

### Core Components

The balancer consists of three main components:
1. WebSocket Balancer (main middleware)
2. Connection Fetcher (metrics collector)
3. WebSocket Relay (message forwarder)

### Configuration Options

```go
type Config struct {
    MetricPath    string        // Path to fetch connection metrics
    Pods          []string      // List of backend pod URLs
    TLSVerify     bool         // Enable/disable TLS verification
    CacheTTL      time.Duration // Metrics cache duration
    DialTimeout   time.Duration // WebSocket connection timeout
    WriteTimeout  time.Duration // Write operation timeout
    ReadTimeout   time.Duration // Read operation timeout
}
```

### Default Values
- MetricPath: "/metric"
- TLSVerify: true
- CacheTTL: 30 seconds
- DialTimeout: 10 seconds
- WriteTimeout: 10 seconds
- ReadTimeout: 30 seconds

## Architecture

### Connection Flow
1. Client initiates WebSocket connection
2. Balancer checks connection counts from all backends
3. Selects backend with lowest connection count
4. Establishes connection to chosen backend
5. Sets up bidirectional relay
6. Monitors connection health

### Load Balancing Algorithm
- Maintains cache of connection counts
- Updates counts based on CacheTTL
- Uses atomic operations for thread safety
- Handles backend failures gracefully

## Development

### Project Structure
```
traefikwsbalancer/
├── cmd/
│   ├── test-client/
│   │   └── main.go
│   └── test-server/
│       └── main.go
├── traefikwsbalancer.go
├── go.mod
└── go.sum
```

### Testing

The project includes several test types:
- Unit tests for core functionality
- Integration tests for WebSocket handling
- Load tests for connection management

Run tests:
```bash
go test ./...
```

## Production Deployment

### Best Practices
1. **Security**
   - Enable TLS verification in production
   - Implement proper origin checking
   - Set appropriate timeouts

2. **Performance**
   - Adjust CacheTTL based on load
   - Monitor connection counts
   - Set appropriate buffer sizes

3. **Monitoring**
   - Log connection events
   - Track metrics
   - Monitor backend health

### Example Configuration

```yaml
wsbalancer:
  metricPath: "/metric"
  pods:
    - "http://backend1:8080"
    - "http://backend2:8080"
  tlsVerify: true
  cacheTTL: "30s"
  dialTimeout: "10s"
  writeTimeout: "10s"
  readTimeout: "30s"
```

## Troubleshooting

### Common Issues

1. **Connection Failures**
   - Check backend availability
   - Verify network connectivity
   - Check TLS configuration

2. **Performance Issues**
   - Monitor connection counts
   - Adjust cache TTL
   - Check backend resources

3. **Protocol Errors**
   - Verify WebSocket upgrade headers
   - Check protocol compatibility
   - Monitor message sizes

### Debugging

Enable detailed logging:
```go
log.SetFlags(log.Lshortfile | log.Ltime | log.Lmicroseconds)
```

## Contributing

1. Fork the repository
2. Create your feature branch
3. Commit your changes
4. Push to the branch
5. Create a Pull Request

## License

MIT License

## Support

For issues and feature requests, please create an issue in the GitHub repository.

## Acknowledgments

- gorilla/websocket team for the excellent WebSocket implementation
- Traefik team for the plugin system
- Contributors who have helped improve this project