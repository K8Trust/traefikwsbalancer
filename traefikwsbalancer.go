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
    if len(config.Services) == 0 {
        return nil, fmt.Errorf("no services configured")
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
    // Select service with least connections
    var selectedService string
    minConnections := int64(^uint64(0) >> 1)

    log.Printf("[DEBUG] Request received: %s %s", req.Method, req.URL.Path)
    log.Printf("[DEBUG] Checking connection counts across services")

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
    // Track connection
    b.trackConnection(targetService, 1)
    defer b.trackConnection(targetService, -1)

    // Configure dialer with timeouts
    dialer := ws.Dialer{
        HandshakeTimeout: b.DialTimeout,
        ReadBufferSize:   4096,
        WriteBufferSize:  4096,
    }

    // Create target URL for the backend service
    targetURL := "ws" + strings.TrimPrefix(targetService, "http") + req.URL.Path
    if req.URL.RawQuery != "" {
        targetURL += "?" + req.URL.RawQuery
    }

    log.Printf("[DEBUG] Dialing backend WebSocket service at %s", targetURL)

    // Clean headers for backend connection
    cleanHeaders := make(http.Header)
    for k, v := range req.Header {
        cleanHeaders[k] = v
    }
    // Ensure required WebSocket headers are present
    cleanHeaders.Set("Upgrade", "websocket")
    cleanHeaders.Set("Connection", "Upgrade")
    if protocol := req.Header.Get("Sec-WebSocket-Protocol"); protocol != "" {
        cleanHeaders.Set("Sec-WebSocket-Protocol", protocol)
}

    // Connect to the backend with retries
    var backendConn *ws.Conn
    var resp *http.Response
    var err error
    
    for retries := 0; retries < 3; retries++ {
        backendConn, resp, err = dialer.Dial(targetURL, cleanHeaders)
        if err == nil {
            break
        }
        log.Printf("[WARN] Retry %d: Failed to connect to backend: %v", retries+1, err)
        time.Sleep(time.Second * time.Duration(retries+1))
    }

    if err != nil {
        log.Printf("[ERROR] Failed to connect to backend after retries: %v", err)
        if resp != nil {
            copyHeaders(rw.Header(), resp.Header)
            rw.WriteHeader(resp.StatusCode)
        } else {
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

    clientConn, err := upgrader.Upgrade(rw, req, nil)
    if err != nil {
        log.Printf("[ERROR] Failed to upgrade client connection: %v", err)
        return
    }
    defer clientConn.Close()

    errChan := make(chan error, 2)

    // Proxy client messages to backend
    go func() {
        for {
            messageType, message, err := clientConn.ReadMessage()
            if err != nil {
                errChan <- err
                return
            }
            err = backendConn.WriteMessage(messageType, message)
            if err != nil {
                errChan <- err
                return
            }
        }
    }()

    // Proxy backend messages to client
    go func() {
        for {
            messageType, message, err := backendConn.ReadMessage()
            if err != nil {
                errChan <- err
                return
            }
            err = clientConn.WriteMessage(messageType, message)
            if err != nil {
                errChan <- err
                return
            }
        }
    }()

    // Wait for either connection to close
    err = <-errChan
    log.Printf("[DEBUG] WebSocket connection closed: %v", err)
}

func (b *Balancer) handleHTTP(rw http.ResponseWriter, req *http.Request, targetService string) {
    // Create proxy request
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

    log.Printf("[DEBUG] Forwarding request to %s", targetURL)
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

func copyHeaders(dst, src http.Header) {
    for k, vv := range src {
        for _, v := range vv {
            dst.Add(k, v)
        }
    }
}