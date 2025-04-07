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

// PodMetricsFetcher interface for getting pod-level metrics
type PodMetricsFetcher interface {
	GetAllPodsForService(string) ([]dashboard.PodMetrics, error)
}

// Config represents the plugin configuration.
type Config struct {
	MetricPath         string   `json:"metricPath,omitempty" yaml:"metricPath"`
	BalancerMetricPath string   `json:"balancerMetricPath,omitempty" yaml:"balancerMetricPath"`
	Services           []string `json:"services,omitempty" yaml:"services"`
	CacheTTL           int      `json:"cacheTTL" yaml:"cacheTTL"` // TTL in seconds
	EnableIPScanning   bool     `json:"enableIPScanning" yaml:"enableIPScanning"`
	DiscoveryTimeout   int      `json:"discoveryTimeout" yaml:"discoveryTimeout"`

	// Prometheus configuration
	PrometheusURL     string `json:"prometheusUrl" yaml:"prometheusUrl"`
	PrometheusMetric  string `json:"prometheusMetric" yaml:"prometheusMetric"`
	ServiceLabelName  string `json:"serviceLabelName" yaml:"serviceLabelName"`
	PodLabelName      string `json:"podLabelName" yaml:"podLabelName"`
	PrometheusTimeout int    `json:"prometheusTimeout" yaml:"prometheusTimeout"`
	UsePrometheus     bool   `json:"usePrometheus" yaml:"usePrometheus"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		MetricPath:         "/metric",
		BalancerMetricPath: "/balancer-metrics",
		CacheTTL:           30, // 30 seconds default
		EnableIPScanning:   false,
		DiscoveryTimeout:   2, // 2 seconds default for discovery requests
		
		// Prometheus defaults
		PrometheusURL:     "http://prometheus-operator-kube-p-prometheus.monitoring.svc.cluster.local:9090",
		PrometheusMetric:  "websocket_connections_total",
		ServiceLabelName:  "service",
		PodLabelName:      "pod",
		PrometheusTimeout: 5, // 5 seconds default for Prometheus API queries
		UsePrometheus:     true, // Enable Prometheus by default
	}
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
	PodFetcher         PodMetricsFetcher
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

	// Create a client for HTTP requests
	client := &http.Client{Timeout: 5 * time.Second}

	// Initialize the balancer
	b := &Balancer{
		Next:               next,
		Name:               name,
		Services:           config.Services,
		Client:             client,
		MetricPath:         config.MetricPath,
		BalancerMetricPath: config.BalancerMetricPath,
		cacheTTL:           time.Duration(config.CacheTTL) * time.Second,
		DialTimeout:        10 * time.Second,
		WriteTimeout:       10 * time.Second,
		ReadTimeout:        30 * time.Second,
		EnableIPScanning:   config.EnableIPScanning,
		DiscoveryTimeout:   time.Duration(config.DiscoveryTimeout) * time.Second,
	}

	// Configure the metrics fetcher based on configuration
	if config.UsePrometheus {
		log.Printf("[INFO] Using Prometheus metrics source at %s", config.PrometheusURL)
		promFetcher := NewPrometheusConnectionFetcher(
			config.PrometheusURL,
			config.PrometheusMetric,
			config.ServiceLabelName,
			config.PodLabelName,
			time.Duration(config.PrometheusTimeout)*time.Second,
		)
		b.Fetcher = promFetcher
		b.PodFetcher = promFetcher
	} else {
		log.Printf("[INFO] Using direct metrics fetching from services")
		// Use the balancer itself as the default fetcher
		b.Fetcher = b
		b.PodFetcher = b
	}

	// Initialize the connection cache immediately
	go func() {
		log.Printf("[INFO] Performing initial connection cache population")
		for _, service := range b.Services {
			b.refreshServiceConnectionCount(service)
		}
		log.Printf("[INFO] Initial connection cache population completed")
	}()

	// Start asynchronous background refresh.
	b.StartBackgroundRefresh()
	return b, nil
}

// GetConnections retrieves the number of connections for a service.
// This implements the ConnectionFetcher interface for direct metrics fetching.
func (b *Balancer) GetConnections(service string) (int, error) {
	count, err := b.getDirectConnections(service)
	return count, err
}

// getDirectConnections retrieves connection counts directly from the service
func (b *Balancer) getDirectConnections(service string) (int, error) {
	log.Printf("[DEBUG] Fetching connections from service %s using metric path %s", service, b.MetricPath)
	resp, err := b.Client.Get(service + b.MetricPath)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch connections from service %s: %v", service, err)
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read response body from service %s: %v", service, err)
		return 0, err
	}

	var podMetrics dashboard.PodMetrics
	if err := json.Unmarshal(body, &podMetrics); err != nil {
		log.Printf("[DEBUG] Failed to decode as pod metrics, trying legacy format: %v", err)
		var legacyMetrics struct {
			AgentsConnections int `json:"agentsConnections"`
		}
		if err := json.Unmarshal(body, &legacyMetrics); err != nil {
			log.Printf("[ERROR] Failed to decode metrics from service %s: %v", service, err)
			return 0, err
		}
		log.Printf("[DEBUG] Service %s reports %d connections (legacy format)",
			service, legacyMetrics.AgentsConnections)
		return legacyMetrics.AgentsConnections, nil
	}

	log.Printf("[DEBUG] Service %s pod %s reports %d connections",
		service, podMetrics.PodName, podMetrics.AgentsConnections)
	return podMetrics.AgentsConnections, nil
}

// GetAllPodsForService discovers all pods behind a service and fetches their metrics.
// This implements the PodMetricsFetcher interface for direct metrics fetching.
func (b *Balancer) GetAllPodsForService(service string) ([]dashboard.PodMetrics, error) {
	// Step 1: Query the service for initial pod metrics.
	log.Printf("[DEBUG] Fetching connections from service %s using metric path %s", service, b.MetricPath)
	resp, err := b.Client.Get(service + b.MetricPath)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch connections from service %s: %v", service, err)
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read response body from service %s: %v", service, err)
		return nil, err
	}

	var initialPodMetrics dashboard.PodMetrics
	if err := json.Unmarshal(body, &initialPodMetrics); err != nil {
		log.Printf("[DEBUG] Failed to decode as pod metrics, trying legacy format: %v", err)
		var legacyMetrics struct {
			AgentsConnections int `json:"agentsConnections"`
		}
		if err := json.Unmarshal(body, &legacyMetrics); err != nil {
			log.Printf("[ERROR] Failed to decode metrics from service %s: %v", service, err)
			return nil, err
		}
		dummyPod := dashboard.PodMetrics{
			AgentsConnections: legacyMetrics.AgentsConnections,
		}
		return []dashboard.PodMetrics{dummyPod}, nil
	}

	// If no pod info is available, return the single metric.
	if initialPodMetrics.PodName == "" || initialPodMetrics.PodIP == "" {
		return []dashboard.PodMetrics{initialPodMetrics}, nil
	}

	log.Printf("[DEBUG] Found pod info, will attempt to discover all pods for service %s", service)
	allPodMetrics := []dashboard.PodMetrics{initialPodMetrics}

	// Extract service DNS name.
	serviceBase := strings.TrimPrefix(service, "http://")
	podName := initialPodMetrics.PodName
	parts := strings.Split(podName, "-")
	if len(parts) < 2 {
		return allPodMetrics, nil
	}
	baseNameParts := parts[:len(parts)-1]
	baseName := strings.Join(baseNameParts, "-")
	serviceParts := strings.Split(serviceBase, ".")
	if len(serviceParts) == 0 {
		return allPodMetrics, nil
	}
	headlessServiceName := serviceParts[0]
	_ = headlessServiceName // currently not used further
	// namespace := "default"
	// if len(serviceParts) > 1 {
	// 	namespace = serviceParts[1]
	// }

	// Create a dedicated discovery client with a shorter timeout.
	discoveryClient := &http.Client{Timeout: b.DiscoveryTimeout}

	// Pattern 1: Try using the endpoints API.
	endpointsURL := fmt.Sprintf("%s/endpoints", service)
	endpointsResp, err := discoveryClient.Get(endpointsURL)
	if err == nil {
		defer endpointsResp.Body.Close()
		endpointsBody, err := io.ReadAll(endpointsResp.Body)
		if err == nil {
			var endpoints struct {
				Pods []struct {
					Name string `json:"name"`
					IP   string `json:"ip"`
				} `json:"pods"`
			}
			if json.Unmarshal(endpointsBody, &endpoints) == nil && len(endpoints.Pods) > 0 {
				for _, pod := range endpoints.Pods {
					if pod.Name == initialPodMetrics.PodName {
						continue
					}
					podURL := fmt.Sprintf("http://%s%s", pod.IP, b.MetricPath)
					podResp, err := discoveryClient.Get(podURL)
					if err == nil {
						defer podResp.Body.Close()
						podBody, err := io.ReadAll(podResp.Body)
						if err == nil {
							var podMetrics dashboard.PodMetrics
							if json.Unmarshal(podBody, &podMetrics) == nil {
								allPodMetrics = append(allPodMetrics, podMetrics)
								log.Printf("[DEBUG] Discovered pod %s with %d connections via endpoints API",
									podMetrics.PodName, podMetrics.AgentsConnections)
							}
						}
					}
				}
				if len(allPodMetrics) > 1 {
					return allPodMetrics, nil
				}
			}
		}
	}

	// Pattern 2: Try direct pod DNS naming.
	for i := 0; i < 10; i++ {
		potentialPodName := fmt.Sprintf("%s-%d", baseName, i)
		if potentialPodName == initialPodMetrics.PodName {
			continue
		}
		potentialPodURL := fmt.Sprintf("http://%s.%s%s", potentialPodName, serviceBase, b.MetricPath)
		log.Printf("[DEBUG] Trying to discover pod via DNS: %s", potentialPodURL)
		podResp, err := discoveryClient.Get(potentialPodURL)
		if err != nil {
			continue
		}
		defer podResp.Body.Close()
		podBody, err := io.ReadAll(podResp.Body)
		if err != nil {
			log.Printf("[ERROR] Failed to read response from pod %s: %v", potentialPodURL, err)
			continue
		}
		var podMetrics dashboard.PodMetrics
		if err := json.Unmarshal(podBody, &podMetrics); err != nil {
			log.Printf("[ERROR] Failed to decode metrics from pod %s: %v", potentialPodURL, err)
			continue
		}
		allPodMetrics = append(allPodMetrics, podMetrics)
		log.Printf("[DEBUG] Discovered pod %s with %d connections via DNS",
			podMetrics.PodName, podMetrics.AgentsConnections)
	}

	// Pattern 3: Try IP scanning as a last resort (if enabled).
	if initialPodMetrics.PodIP != "" && b.EnableIPScanning {
		ipParts := strings.Split(initialPodMetrics.PodIP, ".")
		if len(ipParts) == 4 {
			baseIP := strings.Join(ipParts[:3], ".")
			lastOctet, err := strconv.Atoi(ipParts[3])
			if err == nil {
				for offset := 1; offset <= 10; offset++ {
					// Incrementing.
					potentialIP := fmt.Sprintf("%s.%d", baseIP, lastOctet+offset)
					potentialURL := fmt.Sprintf("http://%s%s", potentialIP, b.MetricPath)
					podResp, err := discoveryClient.Get(potentialURL)
					if err == nil {
						podBody, _ := io.ReadAll(podResp.Body)
						podResp.Body.Close()
						var podMetrics dashboard.PodMetrics
						if err := json.Unmarshal(podBody, &podMetrics); err == nil && podMetrics.PodName != initialPodMetrics.PodName {
							allPodMetrics = append(allPodMetrics, podMetrics)
							log.Printf("[DEBUG] Discovered pod %s with %d connections via IP scanning",
								podMetrics.PodName, podMetrics.AgentsConnections)
						}
					}
					// Decrementing.
					if lastOctet-offset > 0 {
						potentialIP = fmt.Sprintf("%s.%d", baseIP, lastOctet-offset)
						potentialURL = fmt.Sprintf("http://%s%s", potentialIP, b.MetricPath)
						podResp, err = discoveryClient.Get(potentialURL)
						if err == nil {
							podBody, _ := io.ReadAll(podResp.Body)
							podResp.Body.Close()
							var podMetrics dashboard.PodMetrics
							if err := json.Unmarshal(podBody, &podMetrics); err == nil && podMetrics.PodName != initialPodMetrics.PodName {
								allPodMetrics = append(allPodMetrics, podMetrics)
								log.Printf("[DEBUG] Discovered pod %s with %d connections via IP scanning",
									podMetrics.PodName, podMetrics.AgentsConnections)
							}
						}
					}
				}
			}
		}
	}

	return allPodMetrics, nil
}

// getCachedConnections returns the cached connection count for a service.
// Returns 0 connections with an error when the cache is empty for the service.
func (b *Balancer) getCachedConnections(service string) (int, error) {
	if count, ok := b.connCache.Load(service); ok {
		return count.(int), nil
	}
	return 0, fmt.Errorf("no cached count for service %s", service)
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

// SetConnCache sets the connection cache map (used for testing)
func (b *Balancer) SetConnCache(cache *sync.Map) {
	b.connCache = *cache
}

// refreshServiceConnectionCount refreshes cached connection counts for a single service.
func (b *Balancer) refreshServiceConnectionCount(service string) {
	b.updateMutex.Lock()
	defer b.updateMutex.Unlock()

	log.Printf("[DEBUG] Refreshing connection counts for service %s in background", service)
	
	// Use the pod metrics fetcher to get all pod metrics
	allPodMetrics, err := b.PodFetcher.GetAllPodsForService(service)
	if err != nil {
		log.Printf("[ERROR] Error fetching pod metrics for service %s: %v", service, err)
		// On error, if we already have a cached value, keep using it
		if _, exists := b.connCache.Load(service); !exists {
			// If no existing cache entry, initialize with 0 connections
			// This ensures we at least have a value for routing
			b.connCache.Store(service, 0)
			emptyPods := []dashboard.PodMetrics{}
			b.podMetrics.Store(service, emptyPods)
		}
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

	// Check if cache has been initialized for any service
	cacheMiss := true

	for _, service := range b.Services {
		connections, err := b.getCachedConnections(service)
		if err != nil {
			// If cache miss, try to refresh the cache for this service immediately
			if strings.Contains(err.Error(), "no cached count for service") {
				log.Printf("[DEBUG] Cache miss for service %s, refreshing now", service)
				b.refreshServiceConnectionCount(service)
				// Try again after refresh
				connections, err = b.getCachedConnections(service)
				if err != nil {
					log.Printf("[ERROR] Failed to get connections after refresh for service %s: %v", service, err)
					// Use 0 as the connection count when cache is unavailable
					connections = 0
				} else {
					cacheMiss = false
				}
			} else {
				log.Printf("[ERROR] Failed to get connections for service %s: %v", service, err)
				// Use 0 as the connection count on error
				connections = 0
			}
		} else {
			cacheMiss = false
		}

		allServiceConnections[service] = connections
		log.Printf("[DEBUG] Service %s has %d active connections", service, connections)
		if connections < minConnections {
			minConnections = connections
			selectedService = service
			log.Printf("[DEBUG] New minimum found: service %s with %d connections", service, connections)
		}
	}

	// If cache completely empty, select the first service by default
	if cacheMiss && selectedService == "" && len(b.Services) > 0 {
		selectedService = b.Services[0]
		log.Printf("[INFO] Cache not initialized yet, using first service %s as default", selectedService)
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