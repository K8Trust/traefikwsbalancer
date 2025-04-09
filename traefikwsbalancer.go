package traefikwsbalancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
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

	// Add this to your Balancer struct
	lastDeployTime time.Time

	// New fields for Refresh method
	MetricsMutex     sync.RWMutex
	Metrics          map[string]map[string]dashboard.PodMetrics
	CountMutex       sync.RWMutex
	ConnectionCount  map[string]int
	LastRefresh      map[string]time.Time
	RefreshInterval  int
	Done             chan struct{}
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

	// Create a more robust HTTP client with appropriate timeouts
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	
	httpClient := &http.Client{
		Timeout:   time.Duration(config.DiscoveryTimeout) * time.Second,
		Transport: transport,
	}

	b := &Balancer{
		Next:               next,
		Name:               name,
		Services:           config.Services,
		Client:             httpClient,
		MetricPath:         config.MetricPath,
		BalancerMetricPath: config.BalancerMetricPath,
		cacheTTL:           time.Duration(config.CacheTTL) * time.Second,
		DialTimeout:        10 * time.Second,
		WriteTimeout:       10 * time.Second,
		ReadTimeout:        30 * time.Second,
		EnableIPScanning:   config.EnableIPScanning,
		DiscoveryTimeout:   time.Duration(config.DiscoveryTimeout) * time.Second,
		lastDeployTime:     time.Now(),
		Metrics:            make(map[string]map[string]dashboard.PodMetrics),
		ConnectionCount:    make(map[string]int),
		LastRefresh:        make(map[string]time.Time),
		RefreshInterval:    config.CacheTTL,
		Done:               make(chan struct{}),
	}
	
	// Immediate cache population with retries
	for _, service := range config.Services {
		go func(svc string) {
			// Try multiple times to populate cache
			for i := 0; i < 5; i++ {
				log.Printf("[INFO] Initial cache population attempt %d for %s", i+1, svc)
				count, err := b.refreshServiceConnectionCount(svc)
				if err == nil {
					log.Printf("[INFO] Successfully populated cache for %s with %d connections", svc, count)
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
		}(service)
	}
	
	// Start background refresh routines
	go b.Refresh()
	
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
				
				// Try to parse as array of endpoints first
				var endpointsArray []struct {
					Pod struct {
						Name string `json:"name"`
						IP   string `json:"ip"`
						Node string `json:"node"`
					} `json:"pod"`
					Connections int `json:"connections"`
				}
				
				if json.Unmarshal(endpointsBody, &endpointsArray) == nil && len(endpointsArray) > 0 {
					log.Printf("[DEBUG] Successfully parsed endpoints as array with %d items", len(endpointsArray))
					var allPodMetrics []dashboard.PodMetrics
					for _, endpoint := range endpointsArray {
						podMetrics := dashboard.PodMetrics{
							PodName:           endpoint.Pod.Name,
							PodIP:             endpoint.Pod.IP,
							NodeName:          endpoint.Pod.Node,
							AgentsConnections: endpoint.Connections,
						}
						allPodMetrics = append(allPodMetrics, podMetrics)
					}
					return allPodMetrics, nil
				}
				
				// Fall back to single endpoint object if array parsing fails
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

	// Try getting all pods endpoint if available
	log.Printf("[DEBUG] Attempting to use all-pods API for service %s", service)
	allPodsURL := fmt.Sprintf("%s/pods", service)
	allPodsReq, err := http.NewRequest("GET", allPodsURL, nil)
	if err == nil {
		allPodsReq.Header.Set("User-Agent", "TraefikWSBalancer/1.0")
		allPodsReq.Header.Set("Accept", "application/json")
		
		allPodsResp, err := b.Client.Do(allPodsReq)
		if err == nil && allPodsResp.StatusCode == http.StatusOK {
			defer allPodsResp.Body.Close()
			log.Printf("[DEBUG] All-pods API response: status=%d", allPodsResp.StatusCode)
			
			allPodsBody, err := io.ReadAll(allPodsResp.Body)
			if err == nil {
				log.Printf("[DEBUG] All-pods API response body: %s", string(allPodsBody))
				
				var podsArray []dashboard.PodMetrics
				if json.Unmarshal(allPodsBody, &podsArray) == nil && len(podsArray) > 0 {
					log.Printf("[DEBUG] Successfully parsed all-pods as array with %d items", len(podsArray))
					return podsArray, nil
				}
			}
		} else if allPodsResp != nil {
			allPodsResp.Body.Close()
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

	// Extract service DNS name and pod info.
	// Unused for now, but kept for future expansion
	_ = strings.TrimPrefix(service, "http://")
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
	b.CountMutex.RLock()
	defer b.CountMutex.RUnlock()
	
	if count, ok := b.ConnectionCount[service]; ok {
		return count, nil
	}
	return 0, fmt.Errorf("no cached count for service %s", service)
}

// InitializeCacheForTesting is a helper method to set up connection cache for tests.
func (b *Balancer) InitializeCacheForTesting(service string, count int) {
	b.CountMutex.Lock()
	b.ConnectionCount[service] = count
	b.LastRefresh[service] = time.Now()
	b.CountMutex.Unlock()
}

// GetAllCachedConnections returns maps of service connection counts and pod metrics.
func (b *Balancer) GetAllCachedConnections() (map[string]int, map[string][]dashboard.PodMetrics) {
	connections := make(map[string]int)
	podMetricsMap := make(map[string][]dashboard.PodMetrics)
	
	b.CountMutex.RLock()
	for service, count := range b.ConnectionCount {
		connections[service] = count
	}
	b.CountMutex.RUnlock()
	
	b.MetricsMutex.RLock()
	for service, podsMap := range b.Metrics {
		pods := make([]dashboard.PodMetrics, 0, len(podsMap))
		for _, pod := range podsMap {
			pods = append(pods, pod)
		}
		podMetricsMap[service] = pods
	}
	b.MetricsMutex.RUnlock()
	
	// Add any services that might be missing in the metrics map
	for _, service := range b.Services {
		if _, ok := connections[service]; !ok {
			connections[service] = 0
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

// refreshServiceConnectionCount queries the backend service for the current connection count.
func (b *Balancer) refreshServiceConnectionCount(service string) (int, error) {
	podMetrics, err := b.GetAllPodsForService(service)
	if err != nil {
		log.Printf("[ERROR] Failed to get pod metrics for service %s: %v", service, err)
		return 0, err
	}

	// Store pod metrics in the dashboard
	b.MetricsMutex.Lock()
	metrics, exists := b.Metrics[service]
	if !exists {
		metrics = make(map[string]dashboard.PodMetrics)
		b.Metrics[service] = metrics
	}

	var totalConnections int
	log.Printf("[DEBUG] Found %d pods for service %s", len(podMetrics), service)
	for _, podMetric := range podMetrics {
		metrics[podMetric.PodName] = podMetric
		totalConnections += podMetric.AgentsConnections
		log.Printf("[DEBUG] Pod %s has %d connections", podMetric.PodName, podMetric.AgentsConnections)
	}
	b.MetricsMutex.Unlock()

	// Update the connection count for this service with Jerusalem time
	b.CountMutex.Lock()
	b.ConnectionCount[service] = totalConnections
	b.LastRefresh[service] = time.Now() // Keep using UTC internally
	b.CountMutex.Unlock()

	return totalConnections, nil
}

// Refresh periodically refreshes the connection count for all services.
func (b *Balancer) Refresh() {
	ticker := time.NewTicker(time.Duration(b.RefreshInterval) * time.Second)
	forcedFullRefresh := time.NewTicker(30 * time.Second) // Force a full refresh every 30 seconds
	defer ticker.Stop()
	defer forcedFullRefresh.Stop()

	// Immediately populate initial data for all services
	log.Printf("[INFO] Initial population of connection counts for all services")
	for _, service := range b.Services {
		b.refreshServiceConnectionCount(service)
	}

	for {
		select {
		case <-ticker.C:
			for _, service := range b.Services {
				b.CountMutex.RLock()
				lastRefresh, ok := b.LastRefresh[service]
				b.CountMutex.RUnlock()

				now := time.Now()
				if !ok || now.Sub(lastRefresh) > time.Duration(b.RefreshInterval)*time.Second {
					go func(s string) {
						_, err := b.refreshServiceConnectionCount(s)
						if err != nil {
							log.Printf("[ERROR] Failed to refresh connection count for service %s: %v", s, err)
						}
					}(service)
				}
			}
		case <-forcedFullRefresh.C:
			log.Printf("[INFO] Performing forced full refresh of all services")
			for _, service := range b.Services {
				go func(s string) {
					_, err := b.refreshServiceConnectionCount(s)
					if err != nil {
						log.Printf("[ERROR] Failed to refresh connection count for service %s: %v", s, err)
					}
				}(service)
			}
		case <-b.Done:
			return
		}
	}
}

// diagnoseNetworkConnectivity performs basic connectivity checks to help diagnose issues
func (b *Balancer) diagnoseNetworkConnectivity(service string) {
	log.Printf("[DEBUG] Running network diagnostics for %s", service)
	
	// Test direct HTTP connection to health endpoint
	healthURL := service + "/health"
	req, _ := http.NewRequest("GET", healthURL, nil)
	req.Header.Set("User-Agent", "TraefikWSBalancer/1.0")
	resp, err := b.Client.Do(req)
	if err != nil {
		log.Printf("[ERROR] Network diagnostic: Cannot connect to %s: %v", healthURL, err)
	} else {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[DEBUG] Network diagnostic: Connected to %s, status: %d, body: %s", 
			healthURL, resp.StatusCode, string(body))
	}
	
	// Test metric endpoint
	metricURL := service + b.MetricPath
	metricReq, _ := http.NewRequest("GET", metricURL, nil)
	metricReq.Header.Set("User-Agent", "TraefikWSBalancer/1.0")
	metricResp, err := b.Client.Do(metricReq)
	if err != nil {
		log.Printf("[ERROR] Network diagnostic: Cannot connect to %s: %v", metricURL, err)
	} else {
		defer metricResp.Body.Close()
		metricBody, _ := io.ReadAll(metricResp.Body)
		log.Printf("[DEBUG] Network diagnostic: Connected to %s, status: %d, body: %s", 
			metricURL, metricResp.StatusCode, string(metricBody))
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
	
	// Add a direct refresh API endpoint
	if req.URL.Path == b.BalancerMetricPath+"/refresh" {
		log.Printf("[INFO] Explicit cache refresh requested")
		for _, service := range b.Services {
			go b.refreshServiceConnectionCount(service)
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		rw.Write([]byte(`{"status":"refresh started"}`))
		return
	}

	minConnections := int(^uint(0) >> 1)
	var selectedService string

	log.Printf("[DEBUG] Request received: %s %s", req.Method, req.URL.Path)
	log.Printf("[DEBUG] Request headers: %v", req.Header)
	log.Printf("[DEBUG] Checking connection counts across services:")
	allServiceConnections := make(map[string]int)
	
	// Run diagnostics for the first service if needed
	if len(b.Services) > 0 && time.Since(b.lastDeployTime) < 30*time.Second {
		b.diagnoseNetworkConnectivity(b.Services[0])
	}

	// Get current connection counts from the new cache
	b.CountMutex.RLock()
	needsRefresh := true
	for _, service := range b.Services {
		if count, ok := b.ConnectionCount[service]; ok {
			needsRefresh = false
			allServiceConnections[service] = count
			log.Printf("[DEBUG] Service %s has %d active connections", service, count)
			if count < minConnections {
				minConnections = count
				selectedService = service
				log.Printf("[DEBUG] New minimum found: service %s with %d connections", service, count)
			}
		}
	}
	b.CountMutex.RUnlock()

	// If cache is empty, do an immediate refresh and retry once
	if needsRefresh {
		log.Printf("[WARN] Connection cache appears empty, forcing immediate refresh")
		refreshWait := sync.WaitGroup{}
		for _, service := range b.Services {
			refreshWait.Add(1)
			go func(s string) {
				defer refreshWait.Done()
				b.refreshServiceConnectionCount(s)
			}(service)
		}
		
		// Wait for at least one service to refresh, but max 1 second
		waitChan := make(chan struct{})
		go func() {
			refreshWait.Wait()
			close(waitChan)
		}()
		
		select {
		case <-waitChan:
			log.Printf("[DEBUG] Refresh completed")
		case <-time.After(1 * time.Second):
			log.Printf("[WARN] Refresh timeout, proceeding with what we have")
		}
		
		// Try again with refreshed cache
		b.CountMutex.RLock()
		for _, service := range b.Services {
			if count, ok := b.ConnectionCount[service]; ok {
				allServiceConnections[service] = count
				log.Printf("[DEBUG] Service %s has %d active connections (after refresh)", service, count)
				if count < minConnections {
					minConnections = count
					selectedService = service
					log.Printf("[DEBUG] New minimum found: service %s with %d connections", service, count)
				}
			}
		}
		b.CountMutex.RUnlock()
	}

	if selectedService == "" {
		log.Printf("[WARN] No available services with cached metrics, selecting first service as fallback")
		if len(b.Services) > 0 {
			selectedService = b.Services[0]
			// Force immediate refresh for next request
			go b.refreshServiceConnectionCount(selectedService)
		} else {
			http.Error(rw, "No available service", http.StatusServiceUnavailable)
			return
		}
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

	// In ServeHTTP or other methods
	if time.Since(b.lastDeployTime) < 10*time.Second {
		// We're in startup mode - be more aggressive about refreshing
		for _, service := range b.Services {
			go b.refreshServiceConnectionCount(service)
		}
	}
}

// handleMetricRequest responds with current connection metrics in JSON.
func (b *Balancer) handleMetricRequest(rw http.ResponseWriter, req *http.Request) {
	log.Printf("[DEBUG] Handling JSON balancer metrics request")
	
	type ServiceMetric struct {
		URL         string `json:"url"`
		Connections int    `json:"connections"`
	}
	
	b.CountMutex.RLock()
	serviceConnections := make(map[string]int)
	for service, count := range b.ConnectionCount {
		serviceConnections[service] = count
	}
	lastRefresh := make(map[string]string)
	for service, ts := range b.LastRefresh {
		// Format the last refresh time in Jerusalem time
		jerusalemTime := getJerusalemTime(ts)
		lastRefresh[service] = jerusalemTime.Format(time.RFC3339)
	}
	b.CountMutex.RUnlock()
	
	b.MetricsMutex.RLock()
	podMetricsMap := make(map[string][]dashboard.PodMetrics)
	for service, podsMap := range b.Metrics {
		pods := make([]dashboard.PodMetrics, 0, len(podsMap))
		for _, pod := range podsMap {
			pods = append(pods, pod)
		}
		podMetricsMap[service] = pods
	}
	b.MetricsMutex.RUnlock()
	
	response := struct {
		Timestamp         string                            `json:"timestamp"`
		Services          []ServiceMetric                   `json:"services"`
		PodMetrics        map[string][]dashboard.PodMetrics `json:"podMetrics,omitempty"`
		TotalCount        int                               `json:"totalConnections"`
		AgentsConnections int                               `json:"agentsConnections"`
		LastRefresh       map[string]string                 `json:"lastRefresh,omitempty"`
		RefreshInterval   int                               `json:"refreshInterval"`
	}{
		Timestamp:       getJerusalemTime(time.Now()).Format(time.RFC3339),
		Services:        make([]ServiceMetric, 0, len(serviceConnections)),
		PodMetrics:      podMetricsMap,
		LastRefresh:     lastRefresh,
		RefreshInterval: b.RefreshInterval,
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

// getJerusalemTime converts a time to Jerusalem (Israel) time zone
func getJerusalemTime(t time.Time) time.Time {
	loc, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil {
		// Fallback to manual UTC+3 if time zone data not available
		return t.UTC().Add(3 * time.Hour)
	}
	return t.In(loc)
}

// handleHTMLMetricRequest serves the HTML dashboard for the metrics.
func (b *Balancer) handleHTMLMetricRequest(rw http.ResponseWriter, req *http.Request) {
	log.Printf("[DEBUG] Handling HTML balancer metrics request")
	
	b.CountMutex.RLock()
	serviceConnections := make(map[string]int)
	for service, count := range b.ConnectionCount {
		serviceConnections[service] = count
	}
	b.CountMutex.RUnlock()
	
	b.MetricsMutex.RLock()
	podMetricsMap := make(map[string][]dashboard.PodMetrics)
	for service, podsMap := range b.Metrics {
		pods := make([]dashboard.PodMetrics, 0, len(podsMap))
		for _, pod := range podsMap {
			pods = append(pods, pod)
		}
		podMetricsMap[service] = pods
	}
	b.MetricsMutex.RUnlock()
	
	data := dashboard.PrepareMetricsData(
		serviceConnections,
		podMetricsMap,
		b.BalancerMetricPath,
	)
	
	// Add refresh interval information
	data.RefreshInterval = b.RefreshInterval
	data.ForcedRefreshInterval = 30 // Hardcoded value from Refresh method
	
	dashboard.RenderHTML(rw, req, data)
}

// handleWebSocket proxies WebSocket connections.
func (b *Balancer) handleWebSocket(rw http.ResponseWriter, req *http.Request, targetService string) {
	// Add CORS headers for preflight requests
	if req.Method == "OPTIONS" {
		rw.Header().Set("Access-Control-Allow-Origin", "*")
		rw.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		rw.Header().Set("Access-Control-Allow-Headers", 
			"Origin, X-Requested-With, Content-Type, Accept, Authorization, Agent-Id, X-Account")
		rw.WriteHeader(http.StatusOK)
		return
	}
	
	// Check for WebSocket upgrade
	if !ws.IsWebSocketUpgrade(req) {
		log.Printf("[ERROR] Not a WebSocket upgrade request")
		http.Error(rw, "WebSocket upgrade required", http.StatusBadRequest)
		return
	}

	// Extract client-requested subprotocols
	clientProtocols := req.Header.Get("Sec-WebSocket-Protocol")
	var requestedProtocols []string
	if clientProtocols != "" {
		requestedProtocols = strings.Split(clientProtocols, ", ")
		log.Printf("[DEBUG] Client requested WebSocket subprotocols: %v", requestedProtocols)
	}

	// Setup dialer with the same subprotocols the client requested
	dialer := ws.Dialer{
		HandshakeTimeout: b.DialTimeout,
		ReadBufferSize:   1024,
		WriteBufferSize:  1024,
		Subprotocols:     requestedProtocols,
	}

	targetURL := "ws" + strings.TrimPrefix(targetService, "http") + req.URL.Path
	if req.URL.RawQuery != "" {
		targetURL += "?" + req.URL.RawQuery
	}

	log.Printf("[DEBUG] Dialing backend WebSocket service at %s", targetURL)
	log.Printf("[DEBUG] Original request headers: %v", req.Header)

	// Create headers for the backend connection
	cleanHeaders := make(http.Header)
	for k, v := range req.Header {
		// Pass through all headers except those specific to the WebSocket handshake
		switch k {
		case "Upgrade", "Connection", "Sec-Websocket-Key",
			"Sec-Websocket-Version", "Sec-Websocket-Extensions",
			"Sec-Websocket-Protocol":
			continue
		default:
			cleanHeaders[k] = v
		}
	}
	
	// Add test headers to help with debugging/authentication
	// These are required by your Node.js WebSocket server
	if _, exists := cleanHeaders["Agent-Id"]; !exists {
		cleanHeaders.Set("Agent-Id", "test-client") // Test client ID for debugging
	}
	if _, exists := cleanHeaders["Authorization"]; !exists {
		cleanHeaders.Set("Authorization", "Bearer test-token") // Test auth for debugging
	}
	
	// Add origin header if missing (may be required by some WebSocket servers)
	if _, exists := cleanHeaders["Origin"]; !exists {
		host := req.Host
		if host == "" {
			host = "localhost"
		}
		scheme := "https"
		if req.TLS == nil {
			scheme = "http"
		}
		cleanHeaders.Set("Origin", fmt.Sprintf("%s://%s", scheme, host))
	}
	
	log.Printf("[DEBUG] Backend WebSocket request headers: %v", cleanHeaders)

	// Attempt to establish WebSocket connection to backend
	backendConn, resp, err := dialer.Dial(targetURL, cleanHeaders)
	if err != nil {
		log.Printf("[ERROR] Failed to connect to backend WebSocket: %v", err)
		if resp != nil {
			log.Printf("[ERROR] Backend response status: %d", resp.StatusCode)
			log.Printf("[ERROR] Backend response headers: %v", resp.Header)
			
			body, bodyErr := io.ReadAll(resp.Body)
			if bodyErr == nil {
				log.Printf("[ERROR] Backend response body: %s", string(body))
			}
			
			// Add CORS headers to the error response
			rw.Header().Set("Access-Control-Allow-Origin", "*")
			copyHeaders(rw.Header(), resp.Header)
			rw.WriteHeader(resp.StatusCode)
			rw.Write(body)
		} else {
			rw.Header().Set("Access-Control-Allow-Origin", "*")
			http.Error(rw, "Failed to connect to backend", http.StatusBadGateway)
		}
		return
	}
	defer backendConn.Close()

	// Check if backend responded with a subprotocol
	var negotiatedProtocol string
	if resp != nil && resp.Header != nil {
		negotiatedProtocol = resp.Header.Get("Sec-WebSocket-Protocol")
		if negotiatedProtocol != "" {
			log.Printf("[DEBUG] Backend negotiated WebSocket subprotocol: %s", negotiatedProtocol)
		}
	}

	// Add custom upgrader options compatible with Node.js ws
	upgrader := ws.Upgrader{
		HandshakeTimeout: b.DialTimeout,
		ReadBufferSize:   1024,
		WriteBufferSize:  1024,
		CheckOrigin:      func(r *http.Request) bool { return true },
		Subprotocols:     requestedProtocols,
	}

	// Add CORS headers to the WebSocket upgrade response
	responseHeader := http.Header{}
	responseHeader.Set("Access-Control-Allow-Origin", "*")
	
	// Pass along the negotiated subprotocol if any
	if negotiatedProtocol != "" {
		responseHeader.Set("Sec-WebSocket-Protocol", negotiatedProtocol)
	}

	clientConn, err := upgrader.Upgrade(rw, req, responseHeader)
	if err != nil {
		log.Printf("[ERROR] Failed to upgrade client connection: %v", err)
		return
	}
	defer clientConn.Close()
	
	log.Printf("[INFO] Successfully established WebSocket connection to backend and upgraded client connection")

	clientDone := make(chan struct{})
	backendDone := make(chan struct{})

	// Set up ping/pong handlers if needed
	pingInterval := 25 * time.Second
	pingTimer := time.NewTicker(pingInterval)
	defer pingTimer.Stop()
	
	go func() {
		defer close(clientDone)
		defer pingTimer.Stop()
		
		for {
			messageType, message, err := clientConn.ReadMessage()
			if err != nil {
				log.Printf("[DEBUG] Error reading from client: %v", err)
				return
			}
			
			switch messageType {
			case ws.PingMessage:
				// Respond with pong
				if err := clientConn.WriteControl(ws.PongMessage, message, time.Now().Add(10*time.Second)); err != nil {
					log.Printf("[DEBUG] Failed to send pong to client: %v", err)
					return
				}
				continue
			case ws.PongMessage:
				// Received pong, nothing to do
				continue
			case ws.CloseMessage:
				// Client initiated close
				log.Printf("[DEBUG] Client initiated WebSocket close")
				return
			}
			
			log.Printf("[DEBUG] Message from client: type=%d, length=%d", messageType, len(message))
			
			err = backendConn.WriteMessage(messageType, message)
			if err != nil {
				log.Printf("[DEBUG] Error writing to backend: %v", err)
				return
			}
		}
	}()

	go func() {
		defer close(backendDone)
		
		for {
			messageType, message, err := backendConn.ReadMessage()
			if err != nil {
				log.Printf("[DEBUG] Error reading from backend: %v", err)
				return
			}
			
			switch messageType {
			case ws.PingMessage:
				// Respond with pong
				if err := backendConn.WriteControl(ws.PongMessage, message, time.Now().Add(10*time.Second)); err != nil {
					log.Printf("[DEBUG] Failed to send pong to backend: %v", err)
					return
				}
				continue
			case ws.PongMessage:
				// Received pong, nothing to do
				continue
			case ws.CloseMessage:
				// Backend initiated close
				log.Printf("[DEBUG] Backend initiated WebSocket close")
				return
			}
			
			log.Printf("[DEBUG] Message from backend: type=%d, length=%d", messageType, len(message))
			
			err = clientConn.WriteMessage(messageType, message)
			if err != nil {
				log.Printf("[DEBUG] Error writing to client: %v", err)
				return
			}
		}
	}()
	
	// Send periodic pings to keep connection alive
	go func() {
		for {
			select {
			case <-pingTimer.C:
				// Send ping to client
				if err := clientConn.WriteControl(ws.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
					log.Printf("[DEBUG] Failed to send ping to client: %v", err)
					return
				}
				
				// Send ping to backend
				if err := backendConn.WriteControl(ws.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
					log.Printf("[DEBUG] Failed to send ping to backend: %v", err)
					return
				}
			case <-clientDone:
				return
			case <-backendDone:
				return
			}
		}
	}()

	select {
	case <-clientDone:
		log.Printf("[DEBUG] Client WebSocket connection closed")
	case <-backendDone:
		log.Printf("[DEBUG] Backend WebSocket connection closed")
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
