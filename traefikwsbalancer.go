// traefikwsbalancer.go
package traefikwsbalancer

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net/http"
    "strings"
    "sync"
    "sync/atomic"
    "time"

    "github.com/K8Trust/traefikwsbalancer/ws"
)

// Version information. These will be set during build time.
var (
    Version   = "dev"     // Will be set to git tag/commit during build
    BuildTime = "unknown" // Will be set to current time during build
)

type ConnectionFetcher interface {
    GetConnections(string) (int, error)
}

type Config struct {
    MetricPath string   `json:"metricPath,omitempty" yaml:"metricPath"`
    Services   []string `json:"services,omitempty" yaml:"services"`
    CacheTTL   int      `json:"cacheTTL" yaml:"cacheTTL"`
}

func CreateConfig() *Config {
    return &Config{
        MetricPath: "/metric",
        CacheTTL:   30,
    }
}

type Balancer struct {
    Next         http.Handler
    Name         string
    Services     []string
    Client       *http.Client
    MetricPath   string
    Fetcher      ConnectionFetcher
    DialTimeout  time.Duration
    WriteTimeout time.Duration
    ReadTimeout  time.Duration

    activeConns  sync.Map // map[string]*atomic.Int64
    connCache    sync.Map
    cacheTTL     time.Duration
    lastUpdate   time.Time
    updateMutex  sync.Mutex
}

func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
    log.Printf("[INFO] ============= Initializing TraefikWSBalancer Plugin =============")
    log.Printf("[INFO] Version: %s", Version)
    log.Printf("[INFO] Build Time: %s", BuildTime)
    log.Printf("[INFO] Config: %+v", config)

    if len(config.Services) == 0 {
        return nil, fmt.Errorf("no services configured")
    }

    log.Printf("[INFO] Configured Services:")
    for i, service := range config.Services {
        log.Printf("[INFO]   %d. %s", i+1, service)
    }

    b := &Balancer{
        Next:         next,
        Name:         name,
        Services:     config.Services,
        Client:       &http.Client{Timeout: 5 * time.Second},
        MetricPath:   config.MetricPath,
        cacheTTL:     time.Duration(config.CacheTTL) * time.Second,
        DialTimeout:  10 * time.Second,
        WriteTimeout: 10 * time.Second,
        ReadTimeout:  30 * time.Second,
    }

    // Initialize connection counters
    for _, service := range config.Services {
        b.activeConns.Store(service, &atomic.Int64{})
    }

    return b, nil
}

func (b *Balancer) GetConnections(service string) (int, error) {
    if b.Fetcher != nil {
        return b.Fetcher.GetConnections(service)
    }

    resp, err := b.Client.Get(service + b.MetricPath)
    if err != nil {
        return 0, err
    }
    defer resp.Body.Close()

    var metrics struct {
        AgentsConnections int `json:"agentsConnections"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
        return 0, err
    }

    return metrics.AgentsConnections, nil
}

func (b *Balancer) trackConnection(service string, delta int64) {
    if counter, ok := b.activeConns.Load(service); ok {
        counter.(*atomic.Int64).Add(delta)
    }
}

func (b *Balancer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
    log.Printf("[DEBUG] ============= New Request Start =============")
    log.Printf("[DEBUG] Method: %s, Path: %s", req.Method, req.URL.Path)
    log.Printf("[DEBUG] Full URL: %s", req.URL.String())
    log.Printf("[DEBUG] Host: %s", req.Host)
    log.Printf("[DEBUG] Headers: %+v", req.Header)
    var selectedService string
    minConnections := int64(^uint64(0) >> 1)

    log.Printf("[DEBUG] Request received: %s %s", req.Method, req.URL.Path)
    log.Printf("[DEBUG] Request headers: %v", req.Header)

    allServiceConnections := make(map[string]int64)

    for _, service := range b.Services {
        counter, ok := b.activeConns.Load(service)
        if !ok {
            continue
        }
        connections := counter.(*atomic.Int64).Load()
        allServiceConnections[service] = connections

        if connections < minConnections {
            minConnections = connections 
            selectedService = service
        }
    }

    if selectedService == "" {
        log.Printf("[ERROR] No available services found")
        http.Error(rw, "No available service", http.StatusServiceUnavailable)
        return
    }

    log.Printf("[INFO] Selected service %s with %d connections for request %s",
        selectedService, allServiceConnections[selectedService], req.URL.Path)

    if ws.IsWebSocketUpgrade(req) {
        log.Printf("[DEBUG] Handling WebSocket upgrade request")
        b.handleWebSocket(rw, req, selectedService)
        return
    }

    b.handleHTTP(rw, req, selectedService)
}

func (b *Balancer) handleWebSocket(rw http.ResponseWriter, req *http.Request, targetService string) {
    log.Printf("[DEBUG] ============= WebSocket Handling Start =============")
    log.Printf("[DEBUG] Target Service: %s", targetService)
    log.Printf("[DEBUG] Original Headers:")
    for key, values := range req.Header {
        log.Printf("[DEBUG]   %s: %v", key, values)
    }
    log.Printf("[DEBUG] Handling WebSocket request for %s", req.URL.Path)
    log.Printf("[DEBUG] Original request headers: %v", req.Header)

    // Track connection
    b.trackConnection(targetService, 1)
    defer b.trackConnection(targetService, -1)

    // Preserve and forward all headers
    cleanHeaders := make(http.Header)
    for k, v := range req.Header {
        cleanHeaders[k] = v
    }

    // Ensure required WebSocket headers are present
    if cleanHeaders.Get("Upgrade") == "" {
        cleanHeaders.Set("Upgrade", "websocket")
    }
    if cleanHeaders.Get("Connection") == "" {
        cleanHeaders.Set("Connection", "Upgrade")
    }
    if cleanHeaders.Get("Sec-WebSocket-Version") == "" {
        cleanHeaders.Set("Sec-WebSocket-Version", "13")
    }

    log.Printf("[DEBUG] Forwarding headers: %v", cleanHeaders)

    // Create target URL for the backend service
    targetURL := "ws" + strings.TrimPrefix(targetService, "http") + req.URL.Path
    if req.URL.RawQuery != "" {
        targetURL += "?" + req.URL.RawQuery
    }

    log.Printf("[DEBUG] Dialing backend WebSocket service at %s", targetURL)

    // Connect to the backend with retries
    var backendConn *ws.Conn
    var resp *http.Response
    var err error
    
    // Configure dialer with timeouts
    dialer := ws.Dialer{
        HandshakeTimeout: b.DialTimeout,
        ReadBufferSize:   4096,
        WriteBufferSize:  4096,
    }

    log.Printf("[DEBUG] Starting backend connection attempts...")
    for retries := 0; retries < 3; retries++ {
        log.Printf("[DEBUG] Connection attempt %d to %s", retries+1, targetURL)
        backendConn, resp, err = dialer.Dial(targetURL, cleanHeaders)
        if err == nil {
            break
        }
        log.Printf("[WARN] Retry %d: Failed to connect to backend: %v", retries+1, err)
        if resp != nil {
            log.Printf("[WARN] Response status: %d, headers: %v", resp.StatusCode, resp.Header)
            if retries == 2 { // On last retry, forward the error response
                copyHeaders(rw.Header(), resp.Header)
                rw.WriteHeader(resp.StatusCode)
                io.Copy(rw, resp.Body)
            }
            resp.Body.Close()
        }
        time.Sleep(time.Second * time.Duration(retries+1))
    }

    if err != nil {
        log.Printf("[ERROR] ============= Backend Connection Failed =============")
        log.Printf("[ERROR] Failed to connect to backend after retries: %v", err)
        log.Printf("[ERROR] Target URL: %s", targetURL)
        log.Printf("[ERROR] Headers sent: %+v", cleanHeaders)
        if resp == nil {
            http.Error(rw, "Service Unavailable", http.StatusServiceUnavailable)
        }
        return
    }
    defer backendConn.Close()

    // Upgrade the client connection
    upgrader := ws.Upgrader{
        HandshakeTimeout: b.DialTimeout,
        ReadBufferSize:   4096,
        WriteBufferSize:  4096,
        CheckOrigin:      func(r *http.Request) bool { return true },
    }

    log.Printf("[DEBUG] Attempting to upgrade client connection...")
    clientConn, err := upgrader.Upgrade(rw, req, nil)
    if err != nil {
        log.Printf("[ERROR] Failed to upgrade client connection: %v", err)
        return
    }
    defer clientConn.Close()

    log.Printf("[INFO] ============= WebSocket Connection Established =============")
    log.Printf("[INFO] Backend connection: %s", targetURL)
    log.Printf("[INFO] Client Remote Address: %s", req.RemoteAddr)
    log.Printf("[INFO] Current active connections for service: %d", b.getServiceConnections(targetService))

    errChan := make(chan error, 2)
    done := make(chan struct{})
    defer close(done)

    // Forward messages from client to backend
    go func() {
        defer func() {
            if r := recover(); r != nil {
                log.Printf("[ERROR] Panic in client->backend forwarder: %v", r)
                errChan <- fmt.Errorf("panic: %v", r)
            }
        }()

        for {
            select {
            case <-done:
                return
            default:
                messageType, message, err := clientConn.ReadMessage()
                if err != nil {
                    errChan <- fmt.Errorf("client read error: %v", err)
                    return
                }

                backendConn.SetWriteDeadline(time.Now().Add(b.WriteTimeout))
                if err := backendConn.WriteMessage(messageType, message); err != nil {
                    errChan <- fmt.Errorf("backend write error: %v", err)
                    return
                }
            }
        }
    }()

    // Forward messages from backend to client
    go func() {
        defer func() {
            if r := recover(); r != nil {
                log.Printf("[ERROR] Panic in backend->client forwarder: %v", r)
                errChan <- fmt.Errorf("panic: %v", r)
            }
        }()

        for {
            select {
            case <-done:
                return
            default:
                messageType, message, err := backendConn.ReadMessage()
                if err != nil {
                    errChan <- fmt.Errorf("backend read error: %v", err)
                    return
                }

                clientConn.SetWriteDeadline(time.Now().Add(b.WriteTimeout))
                if err := clientConn.WriteMessage(messageType, message); err != nil {
                    errChan <- fmt.Errorf("client write error: %v", err)
                    return
                }
            }
        }
    }()

    // Wait for either connection to close or error
    err = <-errChan
    log.Printf("[DEBUG] WebSocket connection closed: %v", err)
}

func (b *Balancer) handleHTTP(rw http.ResponseWriter, req *http.Request, targetService string) {
    targetURL := targetService + req.URL.Path
    proxyReq, err := http.NewRequest(req.Method, targetURL, req.Body)
    if err != nil {
        log.Printf("[ERROR] Failed to create proxy request: %v", err)
        http.Error(rw, "Failed to create proxy request", http.StatusInternalServerError)
        return
    }

    copyHeaders(proxyReq.Header, req.Header)
    proxyReq.Host = req.Host
    proxyReq.URL.RawQuery = req.URL.RawQuery

    log.Printf("[DEBUG] Forwarding HTTP request to %s", targetURL)
    resp, err := b.Client.Do(proxyReq)
    if err != nil {
        log.Printf("[ERROR] Request failed: %v", err)
        http.Error(rw, "Request failed", http.StatusBadGateway)
        return
    }
    defer resp.Body.Close()

    copyHeaders(rw.Header(), resp.Header)
    rw.WriteHeader(resp.StatusCode)

    if _, err := io.Copy(rw, resp.Body); err != nil {
        log.Printf("[ERROR] Failed to write response body: %v", err)
    }
}

func (b *Balancer) getServiceConnections(service string) int64 {
    if counter, ok := b.activeConns.Load(service); ok {
        return counter.(*atomic.Int64).Load()
    }
    return 0
}

func copyHeaders(dst, src http.Header) {
    for k, vv := range src {
        for _, v := range vv {
            dst.Add(k, v)
        }
    }
}