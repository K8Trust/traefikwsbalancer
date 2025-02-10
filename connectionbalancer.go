package connectionbalancer

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

// ConnectionFetcher is an interface for fetching connection counts.
type ConnectionFetcher interface {
	GetConnections(pod string) (int, error)
}

// DefaultFetcher is the real implementation of ConnectionFetcher.
type DefaultFetcher struct{}

// GetConnections fetches the number of active connections from a pod's metrics endpoint.
func (d *DefaultFetcher) GetConnections(pod string) (int, error) {
	resp, err := http.Get(pod + "/metric")
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

// Config represents the plugin configuration.
type Config struct {
	MetricPath string   `json:"metricPath,omitempty"`
	Pods       []string `json:"pods,omitempty"`
}

// CreateConfig initializes the plugin config.
func CreateConfig() *Config {
	return &Config{
		MetricPath: "/metric",
		Pods: []string{
			"http://kbrain-socket-agent-ktrust-service.phaedra.svc.cluster.local:80",
			"http://kbrain-user-socket-ktrust-service.phaedra.svc.cluster.local:80",
		},
	}
}

// ConnectionBalancer is a middleware plugin.
type ConnectionBalancer struct {
	next       http.Handler
	metricPath string
	client     *http.Client
	fetcher    ConnectionFetcher
	pods       []string
}

// New creates a new ConnectionBalancer middleware.
func New(ctx context.Context, next http.Handler, config *Config, _ string) (http.Handler, error) {
	if len(config.MetricPath) == 0 {
		return nil, fmt.Errorf("metric path cannot be empty")
	}

	return &ConnectionBalancer{
		next:       next,
		metricPath: config.MetricPath,
		client:     &http.Client{},
		fetcher:    &DefaultFetcher{},
		pods:       config.Pods,
	}, nil
}

// ServeHTTP processes incoming requests.
func (cb *ConnectionBalancer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if len(cb.pods) == 0 {
		cb.pods = []string{"http://pod1:8080", "http://pod2:8080"}
	}

	minConnections := int(^uint(0) >> 1)
	var selectedPod string

	for _, pod := range cb.pods {
		connections, err := cb.getConnections(pod)
		if err != nil {
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

	proxyReq, err := http.NewRequest(req.Method, selectedPod+req.URL.Path, req.Body)
	if err != nil {
		http.Error(rw, "Failed to create proxy request", http.StatusInternalServerError)
		return
	}

	proxyReq.Header = req.Header

	resp, err := cb.client.Do(proxyReq)
	if err != nil {
		http.Error(rw, "Request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, v := range resp.Header {
		for _, val := range v {
			rw.Header().Add(k, val)
		}
	}

	rw.WriteHeader(resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	if _, err := rw.Write(body); err != nil {
		fmt.Printf("Failed to write response body: %v\n", err)
	}
}

// getConnections calls the fetcher to get the number of active connections.
func (cb *ConnectionBalancer) getConnections(pod string) (int, error) {
	if cb.fetcher == nil {
		return 0, fmt.Errorf("fetcher is not initialized")
	}
	return cb.fetcher.GetConnections(pod)
}

// isWebSocketRequest checks if the request is a WebSocket upgrade request
func isWebSocketRequest(req *http.Request) bool {
	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")
}

// handleWebSocket handles WebSocket upgrade requests
func (cb *ConnectionBalancer) handleWebSocket(targetPod string, rw http.ResponseWriter, req *http.Request) {
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

	// Create proxy dialer
	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
		HandshakeTimeout: cb.client.Timeout,
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

	// Upgrade the client connection
	clientConn, err := upgrader.Upgrade(rw, req, nil)
	if err != nil {
		http.Error(rw, "Failed to upgrade client connection", http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

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