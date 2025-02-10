package traefikwsbalancer

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ConnectionFetcher is an interface for fetching connection counts.
type ConnectionFetcher interface {
	GetConnections(pod string) (int, error)
}

// DefaultFetcher is the real implementation of ConnectionFetcher.
type DefaultFetcher struct {
	client *http.Client
}

// NewDefaultFetcher creates a new DefaultFetcher with configured timeouts
func NewDefaultFetcher() *DefaultFetcher {
	return &DefaultFetcher{
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// GetConnections fetches the number of active connections from a pod's metrics endpoint.
func (d *DefaultFetcher) GetConnections(pod string) (int, error) {
	resp, err := d.client.Get(pod + "/metric")
	if err != nil {
		return 0, fmt.Errorf("fetching metrics: %w", err)
	}
	defer resp.Body.Close()

	var metrics struct {
		AgentsConnections int `json:"agentsConnections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
		return 0, fmt.Errorf("decoding metrics: %w", err)
	}

	return metrics.AgentsConnections, nil
}

// Config represents the plugin configuration.
type Config struct {
	MetricPath    string        `json:"metricPath,omitempty" yaml:"metricPath"`
	Pods          []string      `json:"pods,omitempty" yaml:"pods"`
	TLSVerify     bool          `json:"tlsVerify" yaml:"tlsVerify"`
	CacheTTL      time.Duration `json:"cacheTTL" yaml:"cacheTTL"`
	DialTimeout   time.Duration `json:"dialTimeout" yaml:"dialTimeout"`
	WriteTimeout  time.Duration `json:"writeTimeout" yaml:"writeTimeout"`
	ReadTimeout   time.Duration `json:"readTimeout" yaml:"readTimeout"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		MetricPath:   "/metric",
		TLSVerify:    true,
		CacheTTL:     30 * time.Second,
		DialTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		ReadTimeout:  30 * time.Second,
	}
}

// WSBalancer is a middleware plugin.
type WSBalancer struct {
	Next       http.Handler
	MetricPath string
	Client     *http.Client
	Fetcher    ConnectionFetcher
	Pods       []string
	TLSVerify  bool
	
	// Connection caching
	connCache    sync.Map // pod -> connCount
	cacheTTL     time.Duration
	lastUpdate   time.Time
	updateMutex  sync.Mutex
	
	// Timeouts
	DialTimeout  time.Duration
	WriteTimeout time.Duration
	ReadTimeout  time.Duration
}

// New creates a new WSBalancer middleware.
func New(ctx context.Context, next http.Handler, config *Config, _ string) (http.Handler, error) {
	if len(config.MetricPath) == 0 {
		return nil, fmt.Errorf("metric path cannot be empty")
	}

	if len(config.Pods) == 0 {
		return nil, fmt.Errorf("no pods configured")
	}

	if !config.TLSVerify {
		log.Printf("WARNING: TLS verification is disabled. This is not recommended for production use.")
	}

	return &WSBalancer{
		Next:         next,
		MetricPath:   config.MetricPath,
		Client:       &http.Client{Timeout: 5 * time.Second},
		Fetcher:      NewDefaultFetcher(),
		Pods:         config.Pods,
		TLSVerify:    config.TLSVerify,
		cacheTTL:     config.CacheTTL,
		DialTimeout:  config.DialTimeout,
		WriteTimeout: config.WriteTimeout,
		ReadTimeout:  config.ReadTimeout,
	}, nil
}

func (cb *WSBalancer) getCachedConnections(pod string) (int, error) {
	cb.updateMutex.Lock()
	defer cb.updateMutex.Unlock()

	if time.Since(cb.lastUpdate) > cb.cacheTTL {
		// Refresh all connection counts
		for _, p := range cb.Pods {
			count, err := cb.Fetcher.GetConnections(p)
			if err != nil {
				log.Printf("Error fetching connections for pod %s: %v", p, err)
				continue
			}
			cb.connCache.Store(p, count)
		}
		cb.lastUpdate = time.Now()
	}

	if count, ok := cb.connCache.Load(pod); ok {
		return count.(int), nil
	}
	return 0, fmt.Errorf("no cached count for pod %s", pod)
}

// ServeHTTP processes incoming requests.
func (cb *WSBalancer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if len(cb.Pods) == 0 {
		http.Error(rw, "No pods configured", http.StatusServiceUnavailable)
		return
	}

	minConnections := int(^uint(0) >> 1)
	var selectedPod string

	for _, pod := range cb.Pods {
		connections, err := cb.getCachedConnections(pod)
		if err != nil {
			log.Printf("Error getting cached connections for pod %s: %v", pod, err)
			continue
		}

		if connections < minConnections {
			minConnections = connections
			selectedPod = pod
		}
	}

	if selectedPod == "" {
		http.Error(rw, "No available pod", http.StatusServiceUnavailable)
		return
	}

	// Check if this is a WebSocket upgrade request
	if isWebSocketRequest(req) {
		cb.handleWebSocket(selectedPod, rw, req)
		return
	}

	// Create new proxy request
	targetURL := selectedPod + req.URL.Path
	proxyReq, err := http.NewRequest(req.Method, targetURL, nil)
	if err != nil {
		http.Error(rw, "Failed to create proxy request", http.StatusInternalServerError)
		return
	}

	// Copy headers
	proxyReq.Header = req.Header.Clone()

	// Copy body if present
	if req.Body != nil {
		proxyReq.Body = req.Body
		proxyReq.GetBody = req.GetBody
		proxyReq.ContentLength = req.ContentLength
	}

	resp, err := cb.Client.Do(proxyReq)
	if err != nil {
		http.Error(rw, "Request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for k, v := range resp.Header {
		for _, val := range v {
			rw.Header().Add(k, val)
		}
	}

	rw.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(rw, resp.Body); err != nil {
		log.Printf("Failed to write response body: %v", err)
	}
}

// isWebSocketRequest checks if the request is a WebSocket upgrade request
func isWebSocketRequest(req *http.Request) bool {
	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")
}

// handleWebSocket handles WebSocket upgrade requests
func (cb *WSBalancer) handleWebSocket(targetPod string, rw http.ResponseWriter, req *http.Request) {
	var upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // In production, you might want to be more restrictive
		},
		EnableCompression: true,
	}

	// Convert http(s) to ws(s)
	targetURL := strings.Replace(targetPod, "http://", "ws://", 1)
	targetURL = strings.Replace(targetURL, "https://", "wss://", 1)
	targetURL = targetURL + req.URL.Path

	// Create proxy dialer with configurable TLS verification
	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: !cb.TLSVerify},
		HandshakeTimeout: cb.DialTimeout,
	}

	// Copy headers for the backend connection
	headers := make(http.Header)
	if agentID := req.Header.Get("Agent-ID"); agentID != "" {
		headers.Set("Agent-ID", agentID)
	}

	// Connect to backend
	backendConn, resp, err := dialer.Dial(targetURL, headers)
	if err != nil {
		if resp != nil {
			copyHeader(rw.Header(), resp.Header)
			rw.WriteHeader(resp.StatusCode)
		} else {
			http.Error(rw, "Failed to connect to WebSocket backend", http.StatusBadGateway)
		}
		return
	}
	defer backendConn.Close()

	// Set read/write deadlines
	backendConn.SetReadDeadline(time.Now().Add(cb.ReadTimeout))
	backendConn.SetWriteDeadline(time.Now().Add(cb.WriteTimeout))

	// Upgrade the client connection
	clientConn, err := upgrader.Upgrade(rw, req, nil)
	if err != nil {
		http.Error(rw, "Failed to upgrade client connection", http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Set read/write deadlines for client connection
	clientConn.SetReadDeadline(time.Now().Add(cb.ReadTimeout))
	clientConn.SetWriteDeadline(time.Now().Add(cb.WriteTimeout))

	// Set up bidirectional message relay
	errChan := make(chan error, 2)
	go relay(clientConn, backendConn, errChan)
	go relay(backendConn, clientConn, errChan)

	// Wait for either connection to close
	<-errChan
}

// relay forwards messages between WebSocket connections
func relay(dest, src *websocket.Conn, errChan chan error) {
	for {
		messageType, message, err := src.ReadMessage()
		if err != nil {
			errChan <- err
			return
		}

		err = dest.WriteMessage(messageType, message)
		if err != nil {
			errChan <- err
			return
		}

		// Update deadlines after successful message transfer
		src.SetReadDeadline(time.Now().Add(30 * time.Second))
		dest.SetWriteDeadline(time.Now().Add(30 * time.Second))
	}
}

// copyHeader copies headers from source to destination
func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}