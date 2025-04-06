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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
	EnablePodMetrics   bool     `json:"enablePodMetrics" yaml:"enablePodMetrics"` // Whether to track per-pod metrics
}

// ServiceDetail holds information about a Kubernetes service
type ServiceDetail struct {
	Name      string
	Namespace string
	Port      int
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		MetricPath:         "/metric",
		BalancerMetricPath: "/balancer-metrics",
		CacheTTL:           30, // 30 seconds default
		EnablePodMetrics:   true,
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
	DialTimeout        time.Duration
	WriteTimeout       time.Duration
	ReadTimeout        time.Duration
	EnablePodMetrics   bool

	// Connection caching.
	connCache         sync.Map // Maps service URL -> connection count
	podConnCache      sync.Map // Maps pod URL -> connection count 
	serviceDetailMap  sync.Map // Maps service URL -> ServiceDetail
	endpointCache     sync.Map // Maps service URL -> []endpoint URLs
	cacheTTL          time.Duration
	lastUpdate        time.Time
	updateMutex       sync.Mutex
	k8sClient         *kubernetes.Clientset
	podDiscoveryError bool
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
		EnablePodMetrics:   config.EnablePodMetrics,
	}

	// Initialize Kubernetes client if pod metrics are enabled
	if config.EnablePodMetrics {
		log.Printf("[INFO] Pod-level metrics are enabled, initializing Kubernetes client")
		k8sConfig, err := rest.InClusterConfig()
		if err != nil {
			log.Printf("[WARN] Failed to create Kubernetes config: %v. Will fall back to service-level metrics only.", err)
			b.podDiscoveryError = true
		} else {
			clientset, err := kubernetes.NewForConfig(k8sConfig)
			if err != nil {
				log.Printf("[WARN] Failed to create Kubernetes client: %v. Will fall back to service-level metrics only.", err)
				b.podDiscoveryError = true
			} else {
				b.k8sClient = clientset
				log.Printf("[INFO] Kubernetes client initialized successfully")
			}
		}
	}

	// Parse and cache service details
	for _, serviceURL := range config.Services {
		if serviceDetail, err := parseServiceURL(serviceURL); err == nil {
			b.serviceDetailMap.Store(serviceURL, serviceDetail)
			log.Printf("[DEBUG] Parsed service URL %s: namespace=%s, name=%s, port=%d", 
				serviceURL, serviceDetail.Namespace, serviceDetail.Name, serviceDetail.Port)
		} else {
			log.Printf("[WARN] Failed to parse service URL %s: %v", serviceURL, err)
		}
	}

	return b, nil
}

// parseServiceURL extracts the service name, namespace, and port from a Kubernetes service URL.
func parseServiceURL(serviceURL string) (ServiceDetail, error) {
	// Remove http:// or https:// prefix
	cleanURL := serviceURL
	if strings.HasPrefix(cleanURL, "http://") {
		cleanURL = strings.TrimPrefix(cleanURL, "http://")
	} else if strings.HasPrefix(cleanURL, "https://") {
		cleanURL = strings.TrimPrefix(cleanURL, "https://")
	}

	// Split into host and port parts
	hostPort := strings.Split(cleanURL, ":")
	if len(hostPort) != 2 {
		return ServiceDetail{}, fmt.Errorf("invalid service URL format: %s", serviceURL)
	}

	host := hostPort[0]
	port := 0
	if _, err := fmt.Sscanf(hostPort[1], "%d", &port); err != nil {
		return ServiceDetail{}, fmt.Errorf("invalid port in service URL: %s", serviceURL)
	}

	// Parse the host part which should be in the format: service-name.namespace.svc.cluster.local
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return ServiceDetail{}, fmt.Errorf("invalid hostname format: %s", host)
	}

	return ServiceDetail{
		Name:      parts[0],
		Namespace: parts[1],
		Port:      port,
	}, nil
}

// getEndpoints fetches the current endpoints for a service.
func (b *Balancer) getEndpoints(serviceDetail ServiceDetail) ([]string, error) {
	if b.k8sClient == nil {
		return nil, fmt.Errorf("kubernetes client not initialized")
	}

	endpoints, err := b.k8sClient.CoreV1().
		Endpoints(serviceDetail.Namespace).
		Get(context.Background(), serviceDetail.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get endpoints: %v", err)
	}

	var endpointURLs []string
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			for _, port := range subset.Ports {
				if int(port.Port) == serviceDetail.Port {
					endpointURL := fmt.Sprintf("http://%s:%d", address.IP, port.Port)
					// Try to get pod info
					if address.TargetRef != nil && address.TargetRef.Kind == "Pod" {
						pod, err := b.k8sClient.CoreV1().
							Pods(serviceDetail.Namespace).
							Get(context.Background(), address.TargetRef.Name, metav1.GetOptions{})
						if err == nil {
							// Store pod info in the URL for display purposes
							endpointURL = fmt.Sprintf("http://%s:%d (pod: %s)", 
								address.IP, port.Port, pod.Name)
						}
					}
					endpointURLs = append(endpointURLs, endpointURL)
				}
			}
		}
	}

	return endpointURLs, nil
}

// GetConnections retrieves the number of connections for a service or pod endpoint.
func (b *Balancer) GetConnections(endpoint string) (int, error) {
	if b.Fetcher != nil {
		log.Printf("[DEBUG] Using custom connection fetcher for endpoint %s", endpoint)
		return b.Fetcher.GetConnections(endpoint)
	}

	// Extract the basic URL without the pod name annotation if present
	endpointURL := endpoint
	if idx := strings.Index(endpoint, " (pod:"); idx > 0 {
		endpointURL = endpoint[:idx]
	}

	log.Printf("[DEBUG] Fetching connections from endpoint %s using metric path %s", endpointURL, b.MetricPath)
	resp, err := b.Client.Get(endpointURL + b.MetricPath)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch connections from endpoint %s: %v", endpointURL, err)
		return 0, err
	}
	defer resp.Body.Close()

	var metrics struct {
		AgentsConnections int `json:"agentsConnections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
		log.Printf("[ERROR] Failed to decode metrics from endpoint %s: %v", endpointURL, err)
		return 0, err
	}

	log.Printf("[DEBUG] Endpoint %s reports %d connections", endpointURL, metrics.AgentsConnections)
	return metrics.AgentsConnections, nil
}

func (b *Balancer) getCachedConnections(service string) (int, error) {
	b.updateMutex.Lock()
	defer b.updateMutex.Unlock()

	if time.Since(b.lastUpdate) > b.cacheTTL {
		log.Printf("[DEBUG] Cache TTL expired, refreshing connection counts")
		
		// Update endpoint cache if pod metrics are enabled
		if b.EnablePodMetrics && b.k8sClient != nil && !b.podDiscoveryError {
			for _, serviceURL := range b.Services {
				if serviceDetail, ok := b.serviceDetailMap.Load(serviceURL); ok {
					endpoints, err := b.getEndpoints(serviceDetail.(ServiceDetail))
					if err != nil {
						log.Printf("[ERROR] Failed to get endpoints for service %s: %v", serviceURL, err)
					} else {
						b.endpointCache.Store(serviceURL, endpoints)
						log.Printf("[DEBUG] Updated endpoints for service %s: %v", serviceURL, endpoints)
					}
				}
			}
		}

		// Update service-level connection counts
		for _, s := range b.Services {
			// Check if we have pod-level metrics for this service
			if endpoints, ok := b.endpointCache.Load(s); ok && b.EnablePodMetrics {
				// Get connections from each endpoint
				totalConnections := 0
				for _, endpoint := range endpoints.([]string) {
					count, err := b.GetConnections(endpoint)
					if err != nil {
						log.Printf("[ERROR] Error fetching connections for endpoint %s: %v", endpoint, err)
						continue
					}
					b.podConnCache.Store(endpoint, count)
					totalConnections += count
				}
				b.connCache.Store(s, totalConnections)
				log.Printf("[DEBUG] Updated cached connections for service %s: %d (from endpoints)", s, totalConnections)
			} else {
				// Fall back to service-level metrics
				count, err := b.GetConnections(s)
				if err != nil {
					log.Printf("[ERROR] Error fetching connections for service %s: %v", s, err)
					continue
				}
				b.connCache.Store(s, count)
				log.Printf("[DEBUG] Updated cached connections for service %s: %d", s, count)
			}
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
func (b *Balancer) GetAllCachedConnections() (map[string]int, map[string]int) {
	b.updateMutex.Lock()
	defer b.updateMutex.Unlock()

	// Refresh connection counts if cache has expired
	if time.Since(b.lastUpdate) > b.cacheTTL {
		log.Printf("[DEBUG] Cache TTL expired, refreshing connection counts")
		
		// Update endpoint cache if pod metrics are enabled
		if b.EnablePodMetrics && b.k8sClient != nil && !b.podDiscoveryError {
			for _, serviceURL := range b.Services {
				if serviceDetail, ok := b.serviceDetailMap.Load(serviceURL); ok {
					endpoints, err := b.getEndpoints(serviceDetail.(ServiceDetail))
					if err != nil {
						log.Printf("[ERROR] Failed to get endpoints for service %s: %v", serviceURL, err)
					} else {
						b.endpointCache.Store(serviceURL, endpoints)
						log.Printf("[DEBUG] Updated endpoints for service %s: %v", serviceURL, endpoints)
					}
				}
			}
		}

		// Update service-level connection counts
		for _, s := range b.Services {
			// Check if we have pod-level metrics for this service
			if endpoints, ok := b.endpointCache.Load(s); ok && b.EnablePodMetrics {
				// Get connections from each endpoint
				totalConnections := 0
				for _, endpoint := range endpoints.([]string) {
					count, err := b.GetConnections(endpoint)
					if err != nil {
						log.Printf("[ERROR] Error fetching connections for endpoint %s: %v", endpoint, err)
						continue
					}
					b.podConnCache.Store(endpoint, count)
					totalConnections += count
				}
				b.connCache.Store(s, totalConnections)
				log.Printf("[DEBUG] Updated cached connections for service %s: %d (from endpoints)", s, totalConnections)
			} else {
				// Fall back to service-level metrics
				count, err := b.GetConnections(s)
				if err != nil {
					log.Printf("[ERROR] Error fetching connections for service %s: %v", s, err)
					continue
				}
				b.connCache.Store(s, count)
				log.Printf("[DEBUG] Updated cached connections for service %s: %d", s, count)
			}
		}
		
		b.lastUpdate = time.Now()
	}

	// Build maps of all service and pod connection counts
	serviceConnections := make(map[string]int)
	podConnections := make(map[string]int)
	
	// Service connections
	for _, service := range b.Services {
		if count, ok := b.connCache.Load(service); ok {
			serviceConnections[service] = count.(int)
		} else {
			serviceConnections[service] = 0
		}
	}
	
	// Pod connections
	if b.EnablePodMetrics {
		b.podConnCache.Range(func(key, value interface{}) bool {
			podConnections[key.(string)] = value.(int)
			return true
		})
	}

	return serviceConnections, podConnections
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
	
	// Get connection counts for all services and pods
	serviceConnections, podConnections := b.GetAllCachedConnections()
	
	// Create response structure
	type ServiceMetric struct {
		URL         string `json:"url"`
		Connections int    `json:"connections"`
	}

	type PodMetric struct {
		URL         string `json:"url"`
		Connections int    `json:"connections"`
		PodName     string `json:"podName,omitempty"`
	}
	
	response := struct {
		Timestamp         string          `json:"timestamp"`
		Services          []ServiceMetric `json:"services"`
		Pods              []PodMetric     `json:"pods,omitempty"`
		TotalCount        int             `json:"totalConnections"`
		AgentsConnections int             `json:"agentsConnections"` // For compatibility with backend metrics
		PodMetricsEnabled bool            `json:"podMetricsEnabled"`
	}{
		Timestamp:         time.Now().Format(time.RFC3339),
		Services:          make([]ServiceMetric, 0, len(serviceConnections)),
		Pods:              make([]PodMetric, 0, len(podConnections)),
		PodMetricsEnabled: b.EnablePodMetrics && !b.podDiscoveryError,
	}
	
	// Fill the services response
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
	
	// Fill the pods response
	for podURL, count := range podConnections {
		podName := ""
		if idx := strings.Index(podURL, " (pod:"); idx > 0 {
			podNamePart := podURL[idx+6:] // Skip " (pod: "
			podName = strings.TrimSuffix(podNamePart, ")")
		}
		
		response.Pods = append(response.Pods, PodMetric{
			URL:         podURL,
			Connections: count,
			PodName:     podName,
		})
	}
	
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