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
	"time"

	"github.com/K8Trust/traefikwsbalancer/ws"
)

// ConnectionFetcher interface for getting connection counts.
type ConnectionFetcher interface {
	GetConnections(string) (int, error)
}

// Config represents the plugin configuration.
type Config struct {
	MetricPath         string   `json:"metricPath,omitempty" yaml:"metricPath"`
	BalancerMetricPath string   `json:"balancerMetricPath,omitempty" yaml:"balancerMetricPath"`
	Services           []string `json:"services,omitempty" yaml:"services"`
	CacheTTL           int      `json:"cacheTTL" yaml:"cacheTTL"` // TTL in seconds
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		MetricPath:         "/metric",
		BalancerMetricPath: "/balancer-metrics",
		CacheTTL:           30, // 30 seconds default
	}
}

// PodMetrics represents metrics response from a pod
type PodMetrics struct {
	AgentsConnections int    `json:"agentsConnections"`
	PodName           string `json:"podName,omitempty"`
	PodIP             string `json:"podIP,omitempty"`
	NodeName          string `json:"nodeName,omitempty"`
}

// Balancer is the connection balancer plugin.
type Balancer struct {
	Next               http.Handler
	Name               string
	Services           []string
	Client             *http.Client
	MetricPath         string
	BalancerMetricPath string
	Fetcher            ConnectionFetcher
	DialTimeout        time.Duration
	WriteTimeout       time.Duration
	ReadTimeout        time.Duration

	// Connection caching.
	connCache    sync.Map
	podMetrics   sync.Map // Maps service -> []PodMetrics
	cacheTTL     time.Duration
	lastUpdate   time.Time
	updateMutex  sync.Mutex
}

// New creates a new plugin instance.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if len(config.Services) == 0 {
		return nil, fmt.Errorf("no services configured")
	}

	log.Printf("[INFO] Creating new balancer instance with %d services", len(config.Services))
	for i, service := range config.Services {
		log.Printf("[DEBUG] Service %d: %s", i+1, service)
	}

	return &Balancer{
		Next:               next,
		Name:               name,
		Services:           config.Services,
		Client:             &http.Client{Timeout: 5 * time.Second},
		MetricPath:         config.MetricPath,
		BalancerMetricPath: config.BalancerMetricPath,
		cacheTTL:           time.Duration(config.CacheTTL) * time.Second,
		DialTimeout:        10 * time.Second,
		WriteTimeout:       10 * time.Second,
		ReadTimeout:        30 * time.Second,
	}, nil
}

// GetConnections retrieves the number of connections for a service
// and pod metadata if available.
func (b *Balancer) GetConnections(service string) (int, []PodMetrics, error) {
	if b.Fetcher != nil {
		log.Printf("[DEBUG] Using custom connection fetcher for service %s", service)
		count, err := b.Fetcher.GetConnections(service)
		return count, nil, err
	}

	log.Printf("[DEBUG] Fetching connections from service %s using metric path %s", service, b.MetricPath)
	resp, err := b.Client.Get(service + b.MetricPath)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch connections from service %s: %v", service, err)
		return 0, nil, err
	}
	defer resp.Body.Close()

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read response body from service %s: %v", service, err)
		return 0, nil, err
	}

	// Try to decode as pod metrics first
	var podMetrics PodMetrics
	if err := json.Unmarshal(body, &podMetrics); err != nil {
		log.Printf("[DEBUG] Failed to decode as pod metrics, trying legacy format: %v", err)
		// If that fails, try the legacy format with just agentsConnections
		var legacyMetrics struct {
			AgentsConnections int `json:"agentsConnections"`
		}
		
		if err := json.Unmarshal(body, &legacyMetrics); err != nil {
			log.Printf("[ERROR] Failed to decode metrics from service %s: %v", service, err)
			return 0, nil, err
		}
		
		log.Printf("[DEBUG] Service %s reports %d connections (legacy format)", 
			service, legacyMetrics.AgentsConnections)
		return legacyMetrics.AgentsConnections, nil, nil
	}

	log.Printf("[DEBUG] Service %s pod %s reports %d connections", 
		service, podMetrics.PodName, podMetrics.AgentsConnections)
	return podMetrics.AgentsConnections, []PodMetrics{podMetrics}, nil
}

func (b *Balancer) getCachedConnections(service string) (int, error) {
	b.updateMutex.Lock()
	defer b.updateMutex.Unlock()

	if time.Since(b.lastUpdate) > b.cacheTTL {
		log.Printf("[DEBUG] Cache TTL expired, refreshing connection counts")
		for _, s := range b.Services {
			count, podMetrics, err := b.GetConnections(s)
			if err != nil {
				log.Printf("[ERROR] Error fetching connections for service %s: %v", s, err)
				continue
			}
			b.connCache.Store(s, count)
			if podMetrics != nil {
				b.podMetrics.Store(s, podMetrics)
			}
			log.Printf("[DEBUG] Updated cached connections for service %s: %d", s, count)
		}
		// Log the current connection counts for all backends
		for _, s := range b.Services {
			if val, ok := b.connCache.Load(s); ok {
				log.Printf("[DEBUG] Cached connection count for backend %s: %d", s, val.(int))
			}
		}
		b.lastUpdate = time.Now()
	}

	if count, ok := b.connCache.Load(service); ok {
		return count.(int), nil
	}
	return 0, fmt.Errorf("no cached count for service %s", service)
}

// GetAllCachedConnections returns a map of all service connection counts
// and pod metrics if available
func (b *Balancer) GetAllCachedConnections() (map[string]int, map[string][]PodMetrics) {
	b.updateMutex.Lock()
	defer b.updateMutex.Unlock()

	// Refresh connection counts if cache has expired
	if time.Since(b.lastUpdate) > b.cacheTTL {
		log.Printf("[DEBUG] Cache TTL expired, refreshing connection counts")
		for _, s := range b.Services {
			count, podMetrics, err := b.GetConnections(s)
			if err != nil {
				log.Printf("[ERROR] Error fetching connections for service %s: %v", s, err)
				continue
			}
			b.connCache.Store(s, count)
			if podMetrics != nil {
				b.podMetrics.Store(s, podMetrics)
			}
			log.Printf("[DEBUG] Updated cached connections for service %s: %d", s, count)
		}
		b.lastUpdate = time.Now()
	}

	// Build maps of service connection counts and pod metrics
	connections := make(map[string]int)
	podMetricsMap := make(map[string][]PodMetrics)
	
	for _, service := range b.Services {
		if count, ok := b.connCache.Load(service); ok {
			connections[service] = count.(int)
		} else {
			connections[service] = 0
		}
		
		if metrics, ok := b.podMetrics.Load(service); ok {
			podMetricsMap[service] = metrics.([]PodMetrics)
		}
	}

	return connections, podMetricsMap
}

func (b *Balancer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// Check if this is a request to our balancer metrics endpoint
	if req.URL.Path == b.BalancerMetricPath {
		b.handleMetricRequest(rw, req)
		return
	}

	// Select service with least connections.
	minConnections := int(^uint(0) >> 1)
	var selectedService string

	log.Printf("[DEBUG] Request received: %s %s", req.Method, req.URL.Path)
	log.Printf("[DEBUG] Checking connection counts across services:")
	allServiceConnections := make(map[string]int)

	for _, service := range b.Services {
		connections, err := b.getCachedConnections(service)
		if err != nil {
			log.Printf("[ERROR] Failed to get connections for service %s: %v", service, err)
			continue
		}

		allServiceConnections[service] = connections
		log.Printf("[DEBUG] Service %s has %d active connections", service, connections)

		if connections < minConnections {
			minConnections = connections
			selectedService = service
			log.Printf("[DEBUG] New minimum found: service %s with %d connections", service, connections)
		}
	}

	if selectedService == "" {
		log.Printf("[ERROR] No available services found")
		http.Error(rw, "No available service", http.StatusServiceUnavailable)
		return
	}

	log.Printf("[INFO] Selected service %s with %d connections (lowest) for request %s. Connection counts: %v",
		selectedService,
		allServiceConnections[selectedService],
		req.URL.Path,
		allServiceConnections)

	// Check if this is a WebSocket upgrade request.
	if ws.IsWebSocketUpgrade(req) {
		log.Printf("[DEBUG] Handling WebSocket upgrade request")
		b.handleWebSocket(rw, req, selectedService)
		return
	}

	// Handle regular HTTP request.
	b.handleHTTP(rw, req, selectedService)
}

// handleMetricRequest responds with current connection metrics for all backends
func (b *Balancer) handleMetricRequest(rw http.ResponseWriter, req *http.Request) {
	log.Printf("[DEBUG] Handling balancer metrics request")
	
	// Get connection counts for all services
	serviceConnections, podMetricsMap := b.GetAllCachedConnections()
	
	// Create response structure
	type ServiceMetric struct {
		URL         string `json:"url"`
		Connections int    `json:"connections"`
	}
	
	response := struct {
		Timestamp         string                  `json:"timestamp"`
		Services          []ServiceMetric         `json:"services"`
		PodMetrics        map[string][]PodMetrics `json:"podMetrics,omitempty"`
		TotalCount        int                     `json:"totalConnections"`
		AgentsConnections int                     `json:"agentsConnections"` // For compatibility with backend metrics
	}{
		Timestamp:  time.Now().Format(time.RFC3339),
		Services:   make([]ServiceMetric, 0, len(serviceConnections)),
		PodMetrics: podMetricsMap,
	}
	
	// Fill the response
	totalConnections := 0
	for service, count := range serviceConnections {
		response.Services = append(response.Services, ServiceMetric{
			URL:         service,
			Connections: count,
		})
		totalConnections += count
	}
	response.TotalCount = totalConnections
	response.AgentsConnections = totalConnections // Match the expected format for compatibility
	
	// Return JSON response
	rw.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(rw).Encode(response); err != nil {
		log.Printf("[ERROR] Failed to encode metrics response: %v", err)
		http.Error(rw, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}

func (b *Balancer) handleWebSocket(rw http.ResponseWriter, req *http.Request, targetService string) {
	// Configure dialer with timeouts.
	dialer := ws.Dialer{
		HandshakeTimeout: b.DialTimeout,
		ReadBufferSize:   1024,
		WriteBufferSize:  1024,
	}

	// Create target URL for the backend service.
	targetURL := "ws" + strings.TrimPrefix(targetService, "http") + req.URL.Path
	if req.URL.RawQuery != "" {
		targetURL += "?" + req.URL.RawQuery
	}

	log.Printf("[DEBUG] Dialing backend WebSocket service at %s", targetURL)

	// Clean headers for backend connection.
	cleanHeaders := make(http.Header)
	for k, v := range req.Header {
		switch k {
		case "Upgrade", "Connection", "Sec-Websocket-Key",
			"Sec-Websocket-Version", "Sec-Websocket-Extensions",
			"Sec-Websocket-Protocol":
			continue
		default:
			cleanHeaders[k] = v
		}
	}

	// Connect to the backend.
	backendConn, resp, err := dialer.Dial(targetURL, cleanHeaders)
	if err != nil {
		log.Printf("[ERROR] Failed to connect to backend WebSocket: %v", err)
		if resp != nil {
			copyHeaders(rw.Header(), resp.Header)
			rw.WriteHeader(resp.StatusCode)
		} else {
			http.Error(rw, "Failed to connect to backend", http.StatusBadGateway)
		}
		return
	}
	defer backendConn.Close()

	// Upgrade the client connection.
	upgrader := ws.Upgrader{
		HandshakeTimeout: b.DialTimeout,
		ReadBufferSize:   1024,
		WriteBufferSize:  1024,
		CheckOrigin:      func(r *http.Request) bool { return true },
	}

	clientConn, err := upgrader.Upgrade(rw, req, nil)
	if err != nil {
		log.Printf("[ERROR] Failed to upgrade client connection: %v", err)
		return
	}
	defer clientConn.Close()

	// Create channels to coordinate connection closure.
	clientDone := make(chan struct{})
	backendDone := make(chan struct{})

	// Proxy client messages to backend.
	go func() {
		defer close(clientDone)
		for {
			messageType, message, err := clientConn.ReadMessage()
			if err != nil {
				return
			}
			err = backendConn.WriteMessage(messageType, message)
			if err != nil {
				return
			}
		}
	}()

	// Proxy backend messages to client.
	go func() {
		defer close(backendDone)
		for {
			messageType, message, err := backendConn.ReadMessage()
			if err != nil {
				return
			}
			err = clientConn.WriteMessage(messageType, message)
			if err != nil {
				return
			}
		}
	}()

	// Wait for either connection to close.
	select {
	case <-clientDone:
	case <-backendDone:
	}
}

func (b *Balancer) handleHTTP(rw http.ResponseWriter, req *http.Request, targetService string) {
	// Create proxy request.
	targetURL := targetService + req.URL.Path
	proxyReq, err := http.NewRequest(req.Method, targetURL, req.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to create proxy request to %s: %v", targetURL, err)
		http.Error(rw, "Failed to create proxy request", http.StatusInternalServerError)
		return
	}

	copyHeaders(proxyReq.Header, req.Header)
	proxyReq.Host = req.Host
	proxyReq.URL.RawQuery = req.URL.RawQuery

	log.Printf("[DEBUG] Forwarding request to %s", targetURL)
	resp, err := b.Client.Do(proxyReq)
	if err != nil {
		log.Printf("[ERROR] Request to %s failed: %v", targetURL, err)
		http.Error(rw, "Request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	log.Printf("[INFO] Successfully proxied request to %s with status %d", targetURL, resp.StatusCode)

	copyHeaders(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(rw, resp.Body); err != nil {
		log.Printf("[ERROR] Failed to write response body from %s: %v", targetURL, err)
	}
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}