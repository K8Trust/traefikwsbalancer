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
func (b *Balancer) GetConnections(service string) (int, []dashboard.PodMetrics, error) {
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
	var podMetrics dashboard.PodMetrics
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
	return podMetrics.AgentsConnections, []dashboard.PodMetrics{podMetrics}, nil
}

// GetAllPodsForService discovers all pods behind a service and fetches their metrics
func (b *Balancer) GetAllPodsForService(service string) ([]dashboard.PodMetrics, error) {
	// Step 1: Query the service to get the initial pod metrics
	log.Printf("[DEBUG] Fetching connections from service %s using metric path %s", service, b.MetricPath)
	resp, err := b.Client.Get(service + b.MetricPath)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch connections from service %s: %v", service, err)
		return nil, err
	}
	defer resp.Body.Close()
	
	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read response body from service %s: %v", service, err)
		return nil, err
	}
	
	// Try to decode initial pod metrics
	var initialPodMetrics dashboard.PodMetrics
	if err := json.Unmarshal(body, &initialPodMetrics); err != nil {
		log.Printf("[DEBUG] Failed to decode as pod metrics, trying legacy format: %v", err)
		// If that fails, try the legacy format with just agentsConnections
		var legacyMetrics struct {
			AgentsConnections int `json:"agentsConnections"`
		}
		
		if err := json.Unmarshal(body, &legacyMetrics); err != nil {
			log.Printf("[ERROR] Failed to decode metrics from service %s: %v", service, err)
			return nil, err
		}
		
		// For legacy metrics, we don't have pod info, so just return a single pod metric
		dummyPod := dashboard.PodMetrics{
			AgentsConnections: legacyMetrics.AgentsConnections,
		}
		return []dashboard.PodMetrics{dummyPod}, nil
	}
	
	// Step 2: If we have pod info, try to discover other pods
	if initialPodMetrics.PodName == "" || initialPodMetrics.PodIP == "" {
		// No pod info available, just return the single metric
		return []dashboard.PodMetrics{initialPodMetrics}, nil
	}
	
	log.Printf("[DEBUG] Found pod info, will attempt to discover all pods for service %s", service)
	
	// Start with the pod we already found
	allPodMetrics := []dashboard.PodMetrics{initialPodMetrics}
	
	// Extract service DNS name for direct pod queries
	serviceBase := strings.TrimPrefix(service, "http://")
	
	// Try to discover pod naming pattern
	podName := initialPodMetrics.PodName
	
	// Most Kubernetes deployments use a pattern like: deployment-name-replicaset-hash-random
	// We need to find the base name (deployment-name-replicaset-hash)
	parts := strings.Split(podName, "-")
	if len(parts) < 2 {
		// Can't determine pattern, just return the single pod we found
		return allPodMetrics, nil
	}
	
	// Assume the last segment is the random part
	baseNameParts := parts[:len(parts)-1]
	baseName := strings.Join(baseNameParts, "-")
	
	// Get the headless service name (typically same as deployment name without the hash)
	// This is for direct pod DNS access: pod-hash.service-name.namespace.svc.cluster.local
	serviceParts := strings.Split(serviceBase, ".")
	if len(serviceParts) == 0 {
		// Can't determine service parts, just return what we have
		return allPodMetrics, nil
	}
	
	headlessServiceName := serviceParts[0]
	namespace := "default"
	if len(serviceParts) > 1 {
		namespace = serviceParts[1]
	}
	
	// Try to discover other pods using direct pod addressing
	// The DNS pattern would be: pod-name.headless-service.namespace.svc.cluster.local
	// But since we don't know if a headless service exists, we'll try various DNS patterns
	
	// Pattern 1: Try using label selector to get all pods in the deployment
	// We use the original service domain but add endpoints path parameter
	endpointsURL := fmt.Sprintf("%s/endpoints", service)
	endpointsResp, err := b.Client.Get(endpointsURL)
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
				// Endpoints API available, use it to get all pod IPs
				for _, pod := range endpoints.Pods {
					// Skip the one we already have
					if pod.Name == initialPodMetrics.PodName {
						continue
					}
					
					// Query the pod directly using its IP
					podURL := fmt.Sprintf("http://%s%s", pod.IP, b.MetricPath)
					podResp, err := b.Client.Get(podURL)
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
				
				// If we found more pods, return them
				if len(allPodMetrics) > 1 {
					return allPodMetrics, nil
				}
			}
		}
	}
	
	// Pattern 2: Try direct pod DNS naming
	// Example: pod-hash-random.service-name.namespace.svc.cluster.local
	// We'll try variations for replica indices
	for i := 0; i < 10; i++ {
		// Generate potential pod name with index
		potentialPodName := fmt.Sprintf("%s-%d", baseName, i)
		
		// Skip if it's the pod we already queried
		if potentialPodName == initialPodMetrics.PodName {
			continue
		}
		
		// Construct potential pod URL with DNS name
		potentialPodURL := fmt.Sprintf("http://%s.%s%s", 
			potentialPodName, serviceBase, b.MetricPath)
		
		log.Printf("[DEBUG] Trying to discover pod via DNS: %s", potentialPodURL)
		
		podResp, err := b.Client.Get(potentialPodURL)
		if err != nil {
			// This is expected for pods that don't exist, continue to next index
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
		
		// Found another pod
		allPodMetrics = append(allPodMetrics, podMetrics)
		log.Printf("[DEBUG] Discovered pod %s with %d connections via DNS", 
			podMetrics.PodName, podMetrics.AgentsConnections)
	}
	
	// Pattern 3: Last resort - try direct IP addressing if we know the IP pattern
	// Kubernetes often assigns IPs sequentially, so we can try adjacent IPs
	if initialPodMetrics.PodIP != "" {
		// Parse IP to find pattern
		ipParts := strings.Split(initialPodMetrics.PodIP, ".")
		if len(ipParts) == 4 {
			// Try adjacent IPs by incrementing/decrementing the last octet
			baseIP := strings.Join(ipParts[:3], ".")
			lastOctet, _ := strconv.Atoi(ipParts[3])
			
			// Try a range of IPs around the known one
			for offset := 1; offset <= 10; offset++ {
				// Try incrementing
				potentialIP := fmt.Sprintf("%s.%d", baseIP, lastOctet+offset)
				potentialURL := fmt.Sprintf("http://%s%s", potentialIP, b.MetricPath)
				
				podResp, err := b.Client.Get(potentialURL)
				if err == nil {
					podBody, _ := io.ReadAll(podResp.Body)
					podResp.Body.Close()
					
					var podMetrics dashboard.PodMetrics
					if json.Unmarshal(podBody, &podMetrics) == nil && podMetrics.PodName != initialPodMetrics.PodName {
						allPodMetrics = append(allPodMetrics, podMetrics)
						log.Printf("[DEBUG] Discovered pod %s with %d connections via IP scanning", 
							podMetrics.PodName, podMetrics.AgentsConnections)
					}
				}
				
				// Try decrementing
				if lastOctet-offset > 0 {
					potentialIP = fmt.Sprintf("%s.%d", baseIP, lastOctet-offset)
					potentialURL = fmt.Sprintf("http://%s%s", potentialIP, b.MetricPath)
					
					podResp, err := b.Client.Get(potentialURL)
					if err == nil {
						podBody, _ := io.ReadAll(podResp.Body)
						podResp.Body.Close()
						
						var podMetrics dashboard.PodMetrics
						if json.Unmarshal(podBody, &podMetrics) == nil && podMetrics.PodName != initialPodMetrics.PodName {
							allPodMetrics = append(allPodMetrics, podMetrics)
							log.Printf("[DEBUG] Discovered pod %s with %d connections via IP scanning", 
								podMetrics.PodName, podMetrics.AgentsConnections)
						}
					}
				}
			}
		}
	}
	
	return allPodMetrics, nil
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
func (b *Balancer) GetAllCachedConnections() (map[string]int, map[string][]dashboard.PodMetrics) {
	b.updateMutex.Lock()
	defer b.updateMutex.Unlock()

	// Refresh connection counts if cache has expired
	if time.Since(b.lastUpdate) > b.cacheTTL {
		log.Printf("[DEBUG] Cache TTL expired, refreshing connection counts")
		for _, s := range b.Services {
			// Use our enhanced pod discovery to get all pods behind this service
			allPodMetrics, err := b.GetAllPodsForService(s)
			if err != nil {
				log.Printf("[ERROR] Error fetching pod metrics for service %s: %v", s, err)
				continue
			}
			
			// Calculate total connections for this service
			totalConnections := 0
			for _, pod := range allPodMetrics {
				totalConnections += pod.AgentsConnections
			}
			
			// Store in cache
			b.connCache.Store(s, totalConnections)
			b.podMetrics.Store(s, allPodMetrics)
			
			log.Printf("[DEBUG] Updated cached connections for service %s: %d connections across %d pods", 
				s, totalConnections, len(allPodMetrics))
		}
		b.lastUpdate = time.Now()
	}

	// Build maps of service connection counts and pod metrics
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

func (b *Balancer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// Check if this is a request to our balancer metrics endpoint
	if req.URL.Path == b.BalancerMetricPath {
		// Check if format=json is specified or if Accept header prefers application/json
		format := req.URL.Query().Get("format")
		acceptHeader := req.Header.Get("Accept")
		
		if format == "json" || strings.Contains(acceptHeader, "application/json") {
			// Use existing JSON handler
			b.handleMetricRequest(rw, req)
		} else {
			// Use new HTML handler for pretty display
			b.handleHTMLMetricRequest(rw, req)
		}
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
	log.Printf("[DEBUG] Handling JSON balancer metrics request")
	
	// Get connection counts for all services, with enhanced pod discovery
	serviceConnections, podMetricsMap := b.GetAllCachedConnections()
	
	// Create response structure
	type ServiceMetric struct {
		URL         string `json:"url"`
		Connections int    `json:"connections"`
	}
	
	response := struct {
		Timestamp         string                           `json:"timestamp"`
		Services          []ServiceMetric                  `json:"services"`
		PodMetrics        map[string][]dashboard.PodMetrics `json:"podMetrics,omitempty"`
		TotalCount        int                              `json:"totalConnections"`
		AgentsConnections int                              `json:"agentsConnections"` // For compatibility with backend metrics
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

// handleHTMLMetricRequest serves the HTML dashboard for the metrics
func (b *Balancer) handleHTMLMetricRequest(rw http.ResponseWriter, req *http.Request) {
	log.Printf("[DEBUG] Handling HTML balancer metrics request")
	
	// Get connection counts for all services
	serviceConnections, podMetricsMap := b.GetAllCachedConnections()
	
	// Prepare metrics data for the dashboard
	data := dashboard.PrepareMetricsData(
		serviceConnections,
		podMetricsMap,
		b.BalancerMetricPath,
	)
	
	// Render the HTML dashboard
	dashboard.RenderHTML(rw, req, data)
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