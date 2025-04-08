package traefikwsbalancer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
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
	// MaxRetries is the maximum number of retries for metrics collection
	MaxRetries int `json:"maxRetries" yaml:"maxRetries"`
	// RetryDelay is the delay between retries in seconds
	RetryDelay int `json:"retryDelay" yaml:"retryDelay"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		MetricPath:         "/metric",
		BalancerMetricPath: "/balancer-metrics",
		CacheTTL:           30, // 30 seconds default
		EnableIPScanning:   false,
		DiscoveryTimeout:   5, // 5 seconds default for discovery requests
		MaxRetries:         3, // 3 retries default
		RetryDelay:         1, // 1 second delay between retries
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
	EnableIPScanning   bool
	DiscoveryTimeout   time.Duration
	MaxRetries         int
	RetryDelay         int
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
		MaxRetries:         config.MaxRetries,
		RetryDelay:         config.RetryDelay,
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
	resp, err := b.Client.Get(service + b.MetricPath)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch connections from service %s: %v", service, err)
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read response body from service %s: %v", service, err)
		return 0, nil, err
	}

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
	log.Printf("[DEBUG] Discovering all pods for service: %s", service)

	// Create a dedicated discovery client with appropriate timeout
	discoveryClient := &http.Client{Timeout: b.DiscoveryTimeout}
	
	// Extract the base service name from the URL
	serviceBase := strings.TrimPrefix(service, "http://")
	serviceParts := strings.Split(serviceBase, ".")
	if len(serviceParts) == 0 {
		return nil, fmt.Errorf("invalid service URL format: %s", service)
	}
	
	serviceName := serviceParts[0]
	namespace := "default"
	if len(serviceParts) > 1 {
		namespace = serviceParts[1]
	}
	
	// STRATEGY 1: Try to use Kubernetes Endpoints API
	log.Printf("[DEBUG] Attempting to discover pods using Kubernetes Endpoints API for %s.%s", serviceName, namespace)
	
	// Construct the endpoint URL for the Kubernetes API
	endpointsURLs := []string{
		fmt.Sprintf("%s/endpoints", service),                          // Custom endpoints API provided by the service
		fmt.Sprintf("http://kubernetes.default.svc/api/v1/namespaces/%s/endpoints/%s", namespace, serviceName), // K8s API direct
	}
	
	var allPodMetrics []dashboard.PodMetrics
	var discoveryErrors []string
	
	for _, endpointsURL := range endpointsURLs {
		log.Printf("[DEBUG] Trying endpoints URL: %s", endpointsURL)
		endpointsResp, err := discoveryClient.Get(endpointsURL)
		if err != nil {
			log.Printf("[DEBUG] Failed to fetch endpoints from %s: %v", endpointsURL, err)
			discoveryErrors = append(discoveryErrors, fmt.Sprintf("Failed to fetch from %s: %v", endpointsURL, err))
			continue
		}
		
		defer endpointsResp.Body.Close()
		endpointsBody, err := io.ReadAll(endpointsResp.Body)
		if err != nil {
			log.Printf("[DEBUG] Failed to read endpoints response: %v", err)
			discoveryErrors = append(discoveryErrors, fmt.Sprintf("Failed to read response from %s: %v", endpointsURL, err))
			continue
		}
		
		// Try parsing as a Kubernetes Endpoints object
		var endpoints struct {
			Kind       string `json:"kind"`
			APIVersion string `json:"apiVersion"`
			Subsets    []struct {
				Addresses []struct {
					IP        string `json:"ip"`
					TargetRef struct {
						Kind      string `json:"kind"`
						Name      string `json:"name"`
						Namespace string `json:"namespace"`
					} `json:"targetRef"`
				} `json:"addresses"`
				Ports []struct {
					Port int `json:"port"`
				} `json:"ports"`
			} `json:"subsets"`
		}
		
		if err := json.Unmarshal(endpointsBody, &endpoints); err == nil && len(endpoints.Subsets) > 0 {
			log.Printf("[DEBUG] Successfully parsed Kubernetes Endpoints object")
			
			// Process all pod addresses
			for _, subset := range endpoints.Subsets {
				port := 80
				if len(subset.Ports) > 0 {
					port = subset.Ports[0].Port
				}
				
				for _, address := range subset.Addresses {
					podName := "unknown"
					if address.TargetRef.Kind == "Pod" {
						podName = address.TargetRef.Name
					}
					
					// Fetch metrics from each pod with retries
					podURL := fmt.Sprintf("http://%s:%d%s", address.IP, port, b.MetricPath)
					log.Printf("[DEBUG] Fetching metrics from pod %s at %s", podName, podURL)
					
					var podMetrics dashboard.PodMetrics
					var metricsErr error
					
					// Implement retry logic
					for retry := 0; retry < b.MaxRetries; retry++ {
						if retry > 0 {
							log.Printf("[DEBUG] Retry %d/%d for pod %s", retry, b.MaxRetries, podName)
							time.Sleep(time.Duration(b.RetryDelay) * time.Second)
						}
						
						podResp, err := discoveryClient.Get(podURL)
						if err != nil {
							metricsErr = err
							continue
						}
						
						podBody, err := io.ReadAll(podResp.Body)
						podResp.Body.Close()
						if err != nil {
							metricsErr = err
							continue
						}
						
						if err := json.Unmarshal(podBody, &podMetrics); err != nil {
							metricsErr = err
							continue
						}
						
						// Successfully got metrics
						metricsErr = nil
						break
					}
					
					if metricsErr != nil {
						log.Printf("[WARN] Failed to fetch metrics from pod %s after %d retries: %v", podName, b.MaxRetries, metricsErr)
						discoveryErrors = append(discoveryErrors, fmt.Sprintf("Failed to fetch metrics from pod %s: %v", podName, metricsErr))
						continue
					}
					
					// Add pod IP and name if not set
					if podMetrics.PodIP == "" {
						podMetrics.PodIP = address.IP
					}
					if podMetrics.PodName == "" {
						podMetrics.PodName = podName
					}
					
					allPodMetrics = append(allPodMetrics, podMetrics)
				}
			}
			
			// If we found any pods, return them even if some failed
			if len(allPodMetrics) > 0 {
				return allPodMetrics, nil
			}
		}
	}
	
	// If we got here, we couldn't discover any pods
	if len(discoveryErrors) > 0 {
		return nil, fmt.Errorf("pod discovery failed: %s", strings.Join(discoveryErrors, "; "))
	}
	
	return nil, fmt.Errorf("no pods discovered for service %s", service)
}

// getCachedConnections returns the cached connection count for a service.
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
	log.Printf("[INFO] Starting background refresh for %d services", len(b.Services))
	
	go func() {
		// Initial refresh for all services immediately at startup
		for _, service := range b.Services {
			b.refreshServiceConnectionCount(service)
		}
		
		// Then continue with regular refresh
		ticker := time.NewTicker(b.cacheTTL / 2) // Refresh more frequently than the TTL
		defer ticker.Stop()

		for range ticker.C {
			if len(b.Services) == 0 {
				continue
			}
			
			// Refresh all services each time
			for _, service := range b.Services {
				go b.refreshServiceConnectionCount(service) // Run each refresh in its own goroutine
			}
			
			log.Printf("[DEBUG] Completed refresh cycle for all %d services", len(b.Services))
		}
	}()
}

// refreshServiceConnectionCount refreshes cached connection counts for a single service.
func (b *Balancer) refreshServiceConnectionCount(service string) {
	b.updateMutex.Lock()
	defer b.updateMutex.Unlock()

	log.Printf("[DEBUG] Refreshing connection counts for service %s in background", service)
	
	// Get metrics for all pods behind this service
	allPodMetrics, err := b.GetAllPodsForService(service)
	if err != nil {
		log.Printf("[ERROR] Error fetching pod metrics for service %s: %v", service, err)
		return
	}

	// Calculate total connections across all pods
	totalConnections := 0
	for _, pod := range allPodMetrics {
		totalConnections += pod.AgentsConnections
	}

	// Store both the total connection count and the pod-level metrics
	b.connCache.Store(service, totalConnections)
	b.podMetrics.Store(service, allPodMetrics)
	b.lastUpdate = time.Now()
	
	log.Printf("[DEBUG] Updated cached connections for service %s: %d connections across %d pods",
		service, totalConnections, len(allPodMetrics))
	
	// Log details for each pod
	for _, pod := range allPodMetrics {
		log.Printf("[DEBUG]   - Pod %s: %d connections (IP: %s, Node: %s)",
			pod.PodName, pod.AgentsConnections, pod.PodIP, pod.NodeName)
	}
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

	log.Printf("[DEBUG] Request received: %s %s", req.Method, req.URL.Path)
	
	// Get all service connection counts and pod metrics
	_, podMetricsMap := b.GetAllCachedConnections()
	
	// Find the pod with the minimum number of connections across all services
	minConnections := int(^uint(0) >> 1)
	var selectedPod dashboard.PodMetrics
	var selectedService string
	
	// First determine if we need to look at pod-level balancing
	hasPodMetrics := false
	for _, pods := range podMetricsMap {
		if len(pods) > 0 {
			hasPodMetrics = true
			break
		}
	}

	if hasPodMetrics {
		// Use pod-level load balancing when pod metrics are available
		log.Printf("[DEBUG] Using pod-level load balancing")
		for service, pods := range podMetricsMap {
			for _, pod := range pods {
				log.Printf("[DEBUG] Service %s, Pod %s has %d active connections", 
					service, pod.PodName, pod.AgentsConnections)
				if pod.AgentsConnections < minConnections {
					minConnections = pod.AgentsConnections
					selectedPod = pod
					selectedService = service
					log.Printf("[DEBUG] New minimum found: pod %s with %d connections", pod.PodName, minConnections)
				}
			}
		}
	} else {
		// Fall back to service-level balancing if pod metrics are not available
		log.Printf("[DEBUG] Using service-level load balancing (no pod metrics available)")
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
				log.Printf("[DEBUG] New minimum found: service %s with %d connections", service, minConnections)
			}
		}
	}

	if selectedService == "" {
		log.Printf("[ERROR] No available services found")
		http.Error(rw, "No available service", http.StatusServiceUnavailable)
		return
	}

	// If we have pod metrics, try to target the specific pod IP
	targetService := selectedService
	if hasPodMetrics && selectedPod.PodIP != "" && selectedPod.PodName != "" {
		// Log the selection for debugging
		log.Printf("[INFO] Selected pod %s (%s) with %d connections (lowest) for request %s",
			selectedPod.PodName, 
			selectedPod.PodIP, 
			selectedPod.AgentsConnections,
			req.URL.Path)
		
		// Get just the hostname part of the service URL
		serviceURL := strings.TrimPrefix(selectedService, "http://")
		parts := strings.Split(serviceURL, ":")
		servicePort := "80"
		if len(parts) > 1 {
			servicePort = parts[1]
		}
		
		// Create a direct URL to the selected pod
		targetService = fmt.Sprintf("http://%s:%s", selectedPod.PodIP, servicePort)
	} else {
		log.Printf("[INFO] Selected service %s with %d connections (lowest) for request %s",
			selectedService,
			minConnections,
			req.URL.Path)
	}

	if ws.IsWebSocketUpgrade(req) {
		log.Printf("[DEBUG] Handling WebSocket upgrade request to %s", targetService)
		b.handleWebSocket(rw, req, targetService)
		return
	}

	b.handleHTTP(rw, req, targetService)
}

// handleMetricRequest responds with current connection metrics in JSON.
func (b *Balancer) handleMetricRequest(rw http.ResponseWriter, req *http.Request) {
	log.Printf("[DEBUG] Handling JSON balancer metrics request")
	serviceConnections, podMetricsMap := b.GetAllCachedConnections()
	
	type ServiceMetric struct {
		URL         string `json:"url"`
		Connections int    `json:"connections"`
	}
	
	// Calculate total connections and pod count for the response
	totalConnections := 0
	totalPods := 0
	for _, pods := range podMetricsMap {
		totalPods += len(pods)
	}
	for _, count := range serviceConnections {
		totalConnections += count
	}
	
	response := struct {
		Timestamp         string                            `json:"timestamp"`
		Services          []ServiceMetric                   `json:"services"`
		PodMetrics        map[string][]dashboard.PodMetrics `json:"podMetrics,omitempty"`
		TotalConnections  int                               `json:"totalConnections"`
		AgentsConnections int                               `json:"agentsConnections"`
	}{
		Timestamp:         time.Now().Format(time.RFC3339),
		Services:          make([]ServiceMetric, 0, len(serviceConnections)),
		PodMetrics:        podMetricsMap,
		TotalConnections:  totalConnections,
		AgentsConnections: totalConnections,
	}
	
	// Add service metrics to the response
	for service, count := range serviceConnections {
		response.Services = append(response.Services, ServiceMetric{
			URL:         service,
			Connections: count,
		})
	}
	
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
	log.Printf("[DEBUG] Handling WebSocket connection to %s", targetService)

	// Use standard library's reverse proxy for WebSocket
	backendURL, err := url.Parse(targetService)
	if err != nil {
		log.Printf("[ERROR] Failed to parse target service URL: %v", err)
		http.Error(rw, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Create a reverse proxy
	proxy := httputil.NewSingleHostReverseProxy(backendURL)
	
	// Set up the director to modify the request
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		originalDirector(r)
		r.URL.Path = req.URL.Path
		r.URL.RawQuery = req.URL.RawQuery
		
		// Copy all headers except those that would be added by the http.ReverseProxy
		for k, vv := range req.Header {
			if k != "Connection" && k != "Upgrade" && 
			   !strings.HasPrefix(k, "Sec-Websocket") {
				r.Header[k] = vv
			}
		}
		
		// Set required WebSocket headers
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
	}
	
	// Configure proxy behavior
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode != http.StatusSwitchingProtocols {
			log.Printf("[WARN] Backend did not upgrade connection: %s", resp.Status)
		}
		return nil
	}
	
	// Standard error handling
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		log.Printf("[ERROR] Proxy error: %v", err)
		http.Error(rw, "Service Unavailable", http.StatusServiceUnavailable)
	}
	
	// Handle retries in case of connection failure
	maxRetries := b.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	
	var proxyErr error
	for retry := 0; retry <= maxRetries; retry++ {
		// Create a special response writer to capture the status code
		statusRecorder := &statusRecorder{
			ResponseWriter: rw,
			status:         http.StatusOK,
		}
		
		if retry > 0 {
			log.Printf("[DEBUG] Retry %d/%d for WebSocket connection", retry, maxRetries)
			time.Sleep(time.Duration(b.RetryDelay) * time.Second)
		}
		
		// Serve the request
		proxy.ServeHTTP(statusRecorder, req)
		
		// If the status code is not an error or if it's 101 (switching protocols), we're done
		if statusRecorder.status < 400 || statusRecorder.status == http.StatusSwitchingProtocols {
			log.Printf("[DEBUG] WebSocket connection established with status %d", statusRecorder.status)
			return
		}
		
		log.Printf("[WARN] WebSocket attempt %d failed with status %d", retry+1, statusRecorder.status)
		proxyErr = fmt.Errorf("backend returned status %d", statusRecorder.status)
	}
	
	// If we get here, all retries failed
	log.Printf("[ERROR] WebSocket connection failed after %d retries: %v", maxRetries, proxyErr)
	http.Error(rw, "Service Unavailable", http.StatusServiceUnavailable)
}

// statusRecorder is a wrapper for http.ResponseWriter that captures the status code
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader captures the status code before passing it to the wrapped ResponseWriter
func (r *statusRecorder) WriteHeader(statusCode int) {
	r.status = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

// Hijack implements the http.Hijacker interface to support WebSocket upgrades
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
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

// InitializeConnectionCacheForTest allows tests to set connection cache values.
// This is exported only for testing.
func (b *Balancer) InitializeConnectionCacheForTest(service string, connections int) {
	b.connCache.Store(service, connections)
}
