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
  - Configurable metrics endpoint for both backend services and the balancer
  - Adjustable timeouts (dial, read, write)
  - TLS/SSL verification options
  - Custom header forwarding

- **Pod Discovery and Monitoring**
  - Multi-level IP scanning for pod discovery across subnets
  - Detailed metrics for individual pods
  - Visualization of connection distribution across pods
  - Enhanced debugging capabilities with pod identification

- **Monitoring & Debugging**
  - Detailed logging of connection events
  - Connection metrics exposure through dedicated endpoint
  - Error tracking and reporting
  - Debug-friendly log messages

## Quick Start

### Prerequisites
- Go 1.22 or higher
- gorilla/websocket v1.5.3 or higher

### Installation

1. Clone the repository:
```bash
git clone https://github.com/K8Trust/traefikwsbalancer.git
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
    MetricPath         string        // Path to fetch connection metrics from backends
    BalancerMetricPath string        // Path to expose balancer's metrics
    Services           []string      // List of backend pod URLs
    CacheTTL           int           // Metrics cache duration in seconds
    EnableIPScanning   bool          // Enable scanning for pods across subnets
    DiscoveryTimeout   int           // Timeout for pod discovery requests in seconds
}
```

### Default Values
- MetricPath: "/metric"
- BalancerMetricPath: "/balancer-metrics"
- CacheTTL: 30 seconds
- EnableIPScanning: false
- DiscoveryTimeout: 2 seconds
- DialTimeout: 10 seconds (internal)
- WriteTimeout: 10 seconds (internal)
- ReadTimeout: 30 seconds (internal)

## Pod Discovery

The plugin uses multiple strategies to discover all pods behind a service:

1. **Endpoints API**: Checks if the service exposes an `/endpoints` endpoint with pod information
2. **Direct DNS**: Attempts to find pods using DNS naming patterns like `service-1`, `service-2`, etc.
3. **IP Scanning**: When enabled, scans neighboring IPs to discover pods:
   - Scans the same subnet (varying the 4th octet)
   - Scans neighboring subnets (varying the 3rd octet by ±5)

For optimal pod discovery in complex environments, enable IP scanning:

```yaml
middleware:
  traefikwsbalancer:
    enableIPScanning: true
    discoveryTimeout: 5  # Increase timeout for better discovery
```

## Backend Implementation

### Implementing the Metrics Endpoint

Each backend service must implement a `/metric` endpoint (or custom path configured in `metricPath`) that returns connection information. For pod-level metrics, the endpoint should include pod identification details.

#### Example in Node.js/Express:

```javascript
app.get("/metric", (req, res) => {
  res.json({
    agentsConnections: activeConnections.size,  // Number of active connections
    podName: process.env.POD_NAME,              // Pod name from Kubernetes
    podIP: process.env.POD_IP,                  // Pod IP from Kubernetes
    nodeName: process.env.NODE_NAME             // Node name from Kubernetes
  });
});
```

#### Example in TypeScript/Express:

```typescript
app.get("/metric", (_req: Request, res: Response) => {
  res.json({
    agentsConnections: agentsConnections.size,  // Number of active connections
    podName: process.env.POD_NAME,              // Pod name from Kubernetes
    podIP: process.env.POD_IP,                  // Pod IP from Kubernetes
    nodeName: process.env.NODE_NAME             // Node name from Kubernetes
  });
});
```

#### Example in Go:

```go
http.HandleFunc("/metric", func(w http.ResponseWriter, r *http.Request) {
    metrics := struct {
        AgentsConnections int    `json:"agentsConnections"`
        PodName           string `json:"podName,omitempty"`
        PodIP             string `json:"podIP,omitempty"`
        NodeName          string `json:"nodeName,omitempty"`
    }{
        AgentsConnections: activeConnections.Count(),
        PodName:           os.Getenv("POD_NAME"),
        PodIP:             os.Getenv("POD_IP"),
        NodeName:          os.Getenv("NODE_NAME"),
    }
    
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(metrics)
})
```

### Connection Tracking

Your backend service should track active WebSocket connections. Here's a simple implementation in Node.js:

```javascript
// Track active connections
const activeConnections = new Set();

// WebSocket server setup
const wss = new WebSocketServer({ server });

wss.on('connection', (ws) => {
  // Add connection to tracking
  activeConnections.add(ws);
  
  ws.on('close', () => {
    // Remove connection from tracking
    activeConnections.delete(ws);
  });
});
```

## Metrics Endpoint

The plugin provides a dedicated metrics endpoint that displays:
- Current timestamp
- List of services with their connection counts
- Pod-level metrics showing connection counts per pod
- Total connection count across all services

### Example Metrics Response

```json
{
  "timestamp": "2025-04-06T20:02:59Z",
  "services": [
    {
      "url": "http://service-1.namespace.svc.cluster.local:80",
      "connections": 42
    }
  ],
  "podMetrics": {
    "http://service-1.namespace.svc.cluster.local:80": [
      {
        "agentsConnections": 25,
        "podName": "service-1-pod-abc123",
        "podIP": "10.0.0.1",
        "nodeName": "node-1"
      },
      {
        "agentsConnections": 17,
        "podName": "service-1-pod-def456",
        "podIP": "10.0.0.2",
        "nodeName": "node-2"
      }
    ]
  },
  "totalConnections": 42,
  "agentsConnections": 42
}
```

## Architecture

### Connection Flow
1. Client initiates WebSocket connection
2. Balancer checks connection counts from all backends
3. Selects backend with lowest connection count
4. Establishes connection to chosen backend
5. Sets up bidirectional relay
6. Monitors connection health

### Pod Discovery and Load Balancing
1. Initial metrics request identifies at least one pod
2. Discovery mechanisms attempt to find all related pods:
   - First tries the endpoints API
   - Then attempts direct DNS discovery
   - Finally uses IP scanning (if enabled)
3. All discovered pods contribute to the service's total connections
4. Clients are directed to the pod with the fewest connections

## Production Deployment

### Best Practices
1. **Security**
   - Implement proper origin checking
   - Set appropriate timeouts
   - Secure the metrics endpoints if needed

2. **Performance**
   - Adjust CacheTTL based on load
   - Enable IP scanning for complete pod discovery
   - Set appropriate discovery timeout (5 seconds recommended)
   - Monitor connection counts

3. **Monitoring**
   - Use the dedicated metrics endpoint to monitor traffic distribution
   - Track pod-level metrics to identify potential hotspots
   - Monitor backend health

### Example Traefik Static Configuration

```yaml
# traefik.yml
experimental:
  plugins:
    traefikwsbalancer:
      moduleName: "github.com/K8Trust/traefikwsbalancer"
      version: "v1.0.0"
```

### Example Kubernetes Configuration

```yaml
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: websocket-balancer
  namespace: default
spec:
  plugin:
    traefikwsbalancer:
      metricPath: /metric
      balancerMetricPath: /balancer-metrics
      services:
        - http://my-websocket-service.default.svc.cluster.local:80
      cacheTTL: 30
      enableIPScanning: true
      discoveryTimeout: 5
```

### IngressRoute Example

```yaml
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: websocket-route
  namespace: default
spec:
  entryPoints:
    - websecure
  routes:
    - match: Host(`ws.example.com`) && PathPrefix(`/socket`)
      kind: Rule
      middlewares:
        - name: websocket-balancer
          namespace: default
      services:
        - name: my-websocket-service
          port: 80
  tls: {}
```

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the MIT License - see the LICENSE file for details.

## Support

For issues and feature requests, please create an issue in the GitHub repository.

## Acknowledgments

- gorilla/websocket team for the excellent WebSocket implementation
- Traefik team for the plugin system
- Contributors who have helped improve this project