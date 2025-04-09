package traefikwsbalancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/K8Trust/traefikwsbalancer/internal/dashboard"
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

	// New configuration options
	EnableIPScanning bool `json:"enableIPScanning" yaml:"enableIPScanning"`
	// DiscoveryTimeout is the timeout (in seconds) for pod discovery requests.
	DiscoveryTimeout int `json:"discoveryTimeout" yaml:"discoveryTimeout"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		MetricPath:         "/metric",
		BalancerMetricPath: "/balancer-metrics",
		CacheTTL:           30, // 30 seconds default
		EnableIPScanning:   false,
		DiscoveryTimeout:   2, // 2 seconds default for discovery requests
	}
}

// PodMetrics represents metrics response from a pod.
type PodMetrics dashboard.PodMetrics

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
	connCache   sync.Map
	podMetrics  sync.Map // Maps service -> []dashboard.PodMetrics
	cacheTTL    time.Duration
	lastUpdate  time.Time
	updateMutex sync.Mutex

	// New fields for discovery configuration.
	EnableIPScanning bool
	DiscoveryTimeout time.Duration
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

	b := &Balancer{
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
		EnableIPScanning:   config.EnableIPScanning,
		DiscoveryTimeout:   time.Duration(config.DiscoveryTimeout) * time.Second,
	}
	// Start asynchronous background cache refresh.
	b.StartBackgroundRefresh()
	return b, nil
}

// GetConnections retrieves the number of connections for a service
// and pod metadata if available.
func (b *Balancer) GetConnections(service string) (int, []dashboard.PodMetrics, error) {
	if b.Fetcher != nil {
		log.Printf("[DEBUG] Using custom connection fetcher for service %s", service)
		count, err := b.Fetcher.GetConnections(service)
		return count, nil, err
	}

	log.Printf("[DEBUG] Fetching connections from service %s using metric path %s", service, b.MetricPath)
	
	// Create a request with explicit headers to help with troubleshooting
	req, err := http.NewRequest("GET", service + b.MetricPath, nil)
	if err != nil {
		log.Printf("[ERROR] Failed to create request for %s: %v", service+b.MetricPath, err)
		return 0, nil, err
	}
	
	req.Header.Set("User-Agent", "TraefikWSBalancer/1.0")
	req.Header.Set("Accept", "application/json")
	
	log.Printf("[DEBUG] Sending request to %s with headers: %v", req.URL.String(), req.Header)
	
	resp, err := b.Client.Do(req)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch connections from service %s: %v", service, err)
		return 0, nil, err
	}
	defer resp.Body.Close()
	
	log.Printf("[DEBUG] Received response from %s: status=%d", service+b.MetricPath, resp.StatusCode)
	
	if resp.StatusCode != http.StatusOK {
		log.Printf("[ERROR] Non-OK status code from %s: %d", service+b.MetricPath, resp.StatusCode)
		return 0, nil, fmt.Errorf("service returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read response body from service %s: %v", service, err)
		return 0, nil, err
	}
	
	log.Printf("[DEBUG] Response body from %s: %s", service+b.MetricPath, string(body))

	var podMetrics dashboard.PodMetrics
	if err := json.Unmarshal(body, &podMetrics); err != nil {
		log.Printf("[DEBUG] Failed to decode as pod metrics, trying legacy format: %v", err)
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
	return podMetrics.AgentsConnections, []dashboard.PodMetrics{podMetrics}, nil
}

// GetAllPodsForService discovers all pods behind a service and fetches their metrics.
func (b *Balancer) GetAllPodsForService(service string) ([]dashboard.PodMetrics, error) {
	// Step 1: First try using the endpoints API which is already implemented in your backend
	log.Printf("[DEBUG] Attempting to use endpoints API for service %s", service)
	endpointsURL := fmt.Sprintf("%s/endpoints", service)
	endpointsReq, err := http.NewRequest("GET", endpointsURL, nil)
	if err == nil {
		endpointsReq.Header.Set("User-Agent", "TraefikWSBalancer/1.0")
		endpointsReq.Header.Set("Accept", "application/json")
		
		endpointsResp, err := b.Client.Do(endpointsReq)
		if err == nil && endpointsResp.StatusCode == http.StatusOK {
			defer endpointsResp.Body.Close()
			log.Printf("[DEBUG] Endpoints API response: status=%d", endpointsResp.StatusCode)
			
			endpointsBody, err := io.ReadAll(endpointsResp.Body)
			if err == nil {
				log.Printf("[DEBUG] Endpoints API response body: %s", string(endpointsBody))
				
				var endpointsData struct {
					Pod struct {
						Name string `json:"name"`
						IP   string `json:"ip"`
						Node string `json:"node"`
					} `json:"pod"`
					Connections int `json:"connections"`
				}
				
				if json.Unmarshal(endpointsBody, &endpointsData) == nil {
					podMetrics := dashboard.PodMetrics{
						PodName:           endpointsData.Pod.Name,
						PodIP:             endpointsData.Pod.IP,
						NodeName:          endpointsData.Pod.Node,
						AgentsConnections: endpointsData.Connections,
					}
					
					log.Printf("[DEBUG] Created pod metrics from endpoints API: %+v", podMetrics)
					return []dashboard.PodMetrics{podMetrics}, nil
				}
			}
		} else if endpointsResp != nil {
			endpointsResp.Body.Close()
		}
	}

	// Step 2: Fall back to querying the service for initial pod metrics.
	log.Printf("[DEBUG] Fetching connections from service %s using metric path %s", service, b.MetricPath)
	metricReq, err := http.NewRequest("GET", service+b.MetricPath, nil)
	if err != nil {
		log.Printf("[ERROR] Failed to create request for %s: %v", service+b.MetricPath, err)
		return nil, err
	}
	
	metricReq.Header.Set("User-Agent", "TraefikWSBalancer/1.0")
	metricReq.Header.Set("Accept", "application/json")
	
	resp, err := b.Client.Do(metricReq)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch connections from service %s: %v", service, err)
		return nil, err
	}
	defer resp.Body.Close()
	
	log.Printf("[DEBUG] Metric endpoint response: status=%d", resp.StatusCode)
	
	if resp.StatusCode != http.StatusOK {
		log.Printf("[ERROR] Non-OK status code from %s: %d", service+b.MetricPath, resp.StatusCode)
		return nil, fmt.Errorf("service returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read response body from service %s: %v", service, err)
		return nil, err
	}
	
	log.Printf("[DEBUG] Metric endpoint response body: %s", string(body))

	var initialPodMetrics dashboard.PodMetrics
	if err := json.Unmarshal(body, &initialPodMetrics); err != nil {
		log.Printf("[DEBUG] Failed to decode as pod metrics, trying legacy format: %v", err)
		var legacyMetrics struct {
			AgentsConnections    int      `json:"agentsConnections"`
			PodName              string   `json:"podName"`
			PodIP                string   `json:"podIP"`
			NodeName             string   `json:"nodeName"`
			TotalConnections     int      `json:"totalConnectionsReceived"`
			ActiveConnections    []string `json:"activeConnections"`
		}
		if err := json.Unmarshal(body, &legacyMetrics); err != nil {
			log.Printf("[ERROR] Failed to decode metrics from service %s: %v", service, err)
			return nil, err
		}
		
		// Use the expanded format from your Node.js server
		initialPodMetrics = dashboard.PodMetrics{
			AgentsConnections: legacyMetrics.AgentsConnections,
			PodName:           legacyMetrics.PodName,
			PodIP:             legacyMetrics.PodIP,
			NodeName:          legacyMetrics.NodeName,
		}
		
		log.Printf("[DEBUG] Parsed pod metrics: %+v", initialPodMetrics)
	}

	// If no pod info is available, return the single metric.
	if initialPodMetrics.PodName == "" || initialPodMetrics.PodIP == "" {
		log.Printf("[WARN] Pod name or IP missing from metrics response")
		return []dashboard.PodMetrics{initialPodMetrics}, nil
	}

	log.Printf("[DEBUG] Found pod info, will attempt to discover all pods for service %s", service)
	allPodMetrics := []dashboard.PodMetrics{initialPodMetrics}

	// Extract service DNS name.
	serviceBase := strings.TrimPrefix(service, "http://")
	podName := initialPodMetrics.PodName
	parts := strings.Split(podName, "-")
	if len(parts) < 2 {
		log.Printf("[INFO] Pod name format not suitable for discovery: %s", podName)
		return allPodMetrics, nil
	}
	
	// This handles pod names like kbrain-socket-agent-6649594bd-8fhj7
	// We try to extract the deployment name
	var baseName string
	if len(parts) >= 3 {
		// For deployment pods, we try to get the base deployment name
		// e.g., "kbrain-socket-agent" from "kbrain-socket-agent-6649594bd-8fhj7"
		if len(parts[len(parts)-1]) == 5 && len(parts[len(parts)-2]) >= 8 {
			// Looks like a ReplicaSet suffix with random chars
			baseName = strings.Join(parts[:len(parts)-2], "-")
			log.Printf("[DEBUG] Extracted deployment name: %s from pod name: %s", baseName, podName)
		} else {
			baseName = strings.Join(parts[:len(parts)-1], "-")
		}
	} else {
		baseNameParts := parts[:len(parts)-1]
		baseName = strings.Join(baseNameParts, "-")
	}
	
	log.Printf("[DEBUG] Using base name %s for pod discovery", baseName)
	
	// Create a dedicated discovery client with a shorter timeout.
	discoveryClient := &http.Client{Timeout: b.DiscoveryTimeout}

	// Enhanced IP scanning
	if initialPodMetrics.PodIP != "" && b.EnableIPScanning {
		log.Printf("[DEBUG] Starting enhanced IP scanning with pod IP: %s", initialPodMetrics.PodIP)
		b.scanAdjacentIPs(discoveryClient, initialPodMetrics, &allPodMetrics)
	}

	return allPodMetrics, nil
}

// scanAdjacentIPs tries to discover pods by scanning IP addresses near the known pod
func (b *Balancer) scanAdjacentIPs(client *http.Client, knownPod dashboard.PodMetrics, podList *[]dashboard.PodMetrics) {
	ipParts := strings.Split(knownPod.PodIP, ".")
	if len(ipParts) != 4 {
		log.Printf("[WARN] Invalid IP format for scanning: %s", knownPod.PodIP)
		return
	}
	
	log.Printf("[DEBUG] Starting IP scanning for pod %s with IP %s", knownPod.PodName, knownPod.PodIP)
	
	// Scan the same subnet (fourth octet variations)
	baseIP := strings.Join(ipParts[:3], ".")
	lastOctet, err := strconv.Atoi(ipParts[3])
	if err == nil {
		for offset := 1; offset <= 10; offset++ {
			// Incrementing
			potentialIP := fmt.Sprintf("%s.%d", baseIP, lastOctet+offset)
			b.scanSingleIP(client, potentialIP, knownPod.PodName, podList)
			
			// Decrementing
			if lastOctet-offset > 0 {
				potentialIP = fmt.Sprintf("%s.%d", baseIP, lastOctet-offset)
				b.scanSingleIP(client, potentialIP, knownPod.PodName, podList)
			}
		}
	}
	
	// Scan neighboring subnets (third octet variations)
	thirdOctet, err := strconv.Atoi(ipParts[2])
	if err == nil {
		log.Printf("[DEBUG] Scanning third octet variations around %d", thirdOctet)
		for thirdOffset := -5; thirdOffset <= 5; thirdOffset++ {
			if thirdOffset == 0 {
				continue // Skip current subnet, handled above
			}
			
			newThirdOctet := thirdOctet + thirdOffset
			if newThirdOctet < 0 || newThirdOctet > 255 {
				continue
			}
			
			log.Printf("[DEBUG] Scanning subnet with third octet: %d", newThirdOctet)
			for fourthOctet := 1; fourthOctet <= 20; fourthOctet++ {
				newIP := fmt.Sprintf("%s.%s.%d.%d", ipParts[0], ipParts[1], newThirdOctet, fourthOctet)
				b.scanSingleIP(client, newIP, knownPod.PodName, podList)
			}
		}
	}
}

// scanSingleIP checks a single IP for a pod with metrics
func (b *Balancer) scanSingleIP(client *http.Client, ip string, knownPodName string, podList *[]dashboard.PodMetrics) {
	potentialURL := fmt.Sprintf("http://%s%s", ip, b.MetricPath)
	
	// Create request with headers
	req, err := http.NewRequest("GET", potentialURL, nil)
	if err != nil {
		return
	}
	
	req.Header.Set("User-Agent", "TraefikWSBalancer/1.0")
	req.Header.Set("Accept", "application/json")
	
	podResp, err := client.Do(req)
	if err != nil {
		return
	}
	defer podResp.Body.Close()
	
	if podResp.StatusCode != http.StatusOK {
		return
	}
	
	podBody, err := io.ReadAll(podResp.Body)
	if err != nil {
		return
	}
	
	var podMetrics dashboard.PodMetrics
	if err := json.Unmarshal(podBody, &podMetrics); err != nil {
		// Try the legacy/expanded format from Node.js
		var legacyMetrics struct {
			AgentsConnections int    `json:"agentsConnections"`
			PodName           string `json:"podName"`
			PodIP             string `json:"podIP"`
			NodeName          string `json:"nodeName"`
		}
		if err := json.Unmarshal(podBody, &legacyMetrics); err != nil {
			return
		}
		podMetrics = dashboard.PodMetrics{
			AgentsConnections: legacyMetrics.AgentsConnections,
			PodName:           legacyMetrics.PodName,
			PodIP:             legacyMetrics.PodIP,
			NodeName:          legacyMetrics.NodeName,
		}
	}
	
	// Only add if it's not the pod we already know
	if podMetrics.PodName != "" && podMetrics.PodName != knownPodName {
		*podList = append(*podList, podMetrics)
		log.Printf("[DEBUG] Discovered pod %s with %d connections via IP scanning at %s",
			podMetrics.PodName, podMetrics.AgentsConnections, ip)
	}
}

// getCachedConnections returns the cached connection count for a service.
func (b *Balancer) getCachedConnections(service string) (int, error) {
	if count, ok := b.connCache.Load(service); ok {
		return count.(int), nil
	}
	return 0, fmt.Errorf("no cached count for service %s", service)
}

// InitializeCacheForTesting is a helper method to set up connection cache for tests.
func (b *Balancer) InitializeCacheForTesting(service string, count int) {
	b.connCache.Store(service, count)
}

// GetAllCachedConnections returns maps of service connection counts and pod metrics.
func (b *Balancer) GetAllCachedConnections() (map[string]int, map[string][]dashboard.PodMetrics) {
	connections := make(map[string]int)
	podMetricsMap := make(map[string][]dashboard.PodMetrics)
	for _, service := range b.Services {
		if count, ok := b.connCache.Load(service); ok {
			connections[service] = count.(int)
		} else {
			connections[service] = 0
		}
		if metrics, ok := b.podMetrics.Load(service); ok {
			podMetricsMap[service] = metrics.([]dashboard.PodMetrics)
		}
	}
	return connections, podMetricsMap
}

// StartBackgroundRefresh initiates an asynchronous refresh of connection counts.
func (b *Balancer) StartBackgroundRefresh() {
	go func() {
		ticker := time.NewTicker(b.cacheTTL / 10) // More frequent checks.
		defer ticker.Stop()

		serviceIndex := 0
		for range ticker.C {
			if len(b.Services) == 0 {
				continue
			}
			serviceIndex = (serviceIndex + 1) % len(b.Services)
			service := b.Services[serviceIndex]
			b.refreshServiceConnectionCount(service)
		}
	}()
}

// refreshServiceConnectionCount refreshes cached connection counts for a single service.
func (b *Balancer) refreshServiceConnectionCount(service string) {
	b.updateMutex.Lock()
	defer b.updateMutex.Unlock()

	log.Printf("[DEBUG] Refreshing connection counts for service %s in background", service)
	allPodMetrics, err := b.GetAllPodsForService(service)
	if err != nil {
		log.Printf("[ERROR] Error fetching pod metrics for service %s: %v", service, err)
		return
	}

	totalConnections := 0
	for _, pod := range allPodMetrics {
		totalConnections += pod.AgentsConnections
	}

	b.connCache.Store(service, totalConnections)
	b.podMetrics.Store(service, allPodMetrics)
	b.lastUpdate = time.Now()
	log.Printf("[DEBUG] Updated cached connections for service %s: %d connections across %d pods",
		service, totalConnections, len(allPodMetrics))
}

// ServeHTTP handles incoming HTTP requests.
func (b *Balancer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.URL.Path == b.BalancerMetricPath {
		format := req.URL.Query().Get("format")
		acceptHeader := req.Header.Get("Accept")
		if format == "json" || strings.Contains(acceptHeader, "application/json") {
			b.handleMetricRequest(rw, req)
		} else {
			b.handleHTMLMetricRequest(rw, req)
		}
		return
	}

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

	if ws.IsWebSocketUpgrade(req) {
		log.Printf("[DEBUG] Handling WebSocket upgrade request")
		b.handleWebSocket(rw, req, selectedService)
		return
	}

	b.handleHTTP(rw, req, selectedService)
}

// handleMetricRequest responds with current connection metrics in JSON.
func (b *Balancer) handleMetricRequest(rw http.ResponseWriter, req *http.Request) {
	log.Printf("[DEBUG] Handling JSON balancer metrics request")
	serviceConnections, podMetricsMap := b.GetAllCachedConnections()
	type ServiceMetric struct {
		URL         string `json:"url"`
		Connections int    `json:"connections"`
	}
	response := struct {
		Timestamp         string                            `json:"timestamp"`
		Services          []ServiceMetric                   `json:"services"`
		PodMetrics        map[string][]dashboard.PodMetrics `json:"podMetrics,omitempty"`
		TotalCount        int                               `json:"totalConnections"`
		AgentsConnections int                               `json:"agentsConnections"`
	}{
		Timestamp:  time.Now().Format(time.RFC3339),
		Services:   make([]ServiceMetric, 0, len(serviceConnections)),
		PodMetrics: podMetricsMap,
	}
	totalConnections := 0
	for service, count := range serviceConnections {
		response.Services = append(response.Services, ServiceMetric{
			URL:         service,
			Connections: count,
		})
		totalConnections += count
	}
	response.TotalCount = totalConnections
	response.AgentsConnections = totalConnections

	rw.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(rw).Encode(response); err != nil {
		log.Printf("[ERROR] Failed to encode metrics response: %v", err)
		http.Error(rw, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}

// handleHTMLMetricRequest serves the HTML dashboard for the metrics.
func (b *Balancer) handleHTMLMetricRequest(rw http.ResponseWriter, req *http.Request) {
	log.Printf("[DEBUG] Handling HTML balancer metrics request")
	serviceConnections, podMetricsMap := b.GetAllCachedConnections()
	data := dashboard.PrepareMetricsData(
		serviceConnections,
		podMetricsMap,
		b.BalancerMetricPath,
	)
	dashboard.RenderHTML(rw, req, data)
}

// handleWebSocket proxies WebSocket connections.
func (b *Balancer) handleWebSocket(rw http.ResponseWriter, req *http.Request, targetService string) {
	dialer := ws.Dialer{
		HandshakeTimeout: b.DialTimeout,
		ReadBufferSize:   1024,
		WriteBufferSize:  1024,
	}

	targetURL := "ws" + strings.TrimPrefix(targetService, "http") + req.URL.Path
	if req.URL.RawQuery != "" {
		targetURL += "?" + req.URL.RawQuery
	}

	log.Printf("[DEBUG] Dialing backend WebSocket service at %s", targetURL)

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

	clientDone := make(chan struct{})
	backendDone := make(chan struct{})

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

	select {
	case <-clientDone:
	case <-backendDone:
	}
}

// handleHTTP proxies regular HTTP requests.
func (b *Balancer) handleHTTP(rw http.ResponseWriter, req *http.Request, targetService string) {
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
