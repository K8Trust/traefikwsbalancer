package traefikwsbalancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
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
	// Create a wrapper function with the expected signature for Yaegi compatibility
	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialer := &net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		return dialer.DialContext(ctx, network, addr)
	}

	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
		DialContext:         dialContext,
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
	
	// Run initial diagnostics for each service
	for _, service := range config.Services {
		go func(svc string) {
			log.Printf("[INFO] Running initial diagnostics for service: %s", svc)
			b.runDetailedDiagnostics(svc)
		}(service)
	}
	
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
	log.Printf("[DEBUG] Endpoints URL: %s", endpointsURL)
	
	endpointsReq, err := http.NewRequest("GET", endpointsURL, nil)
	if err != nil {
		log.Printf("[ERROR] Failed to create endpoints request: %v", err)
		// Continue to next approach instead of returning
	} else {
		endpointsReq.Header.Set("User-Agent", "TraefikWSBalancer/1.0")
		endpointsReq.Header.Set("Accept", "application/json")
		
		log.Printf("[DEBUG] Sending endpoints API request to %s", endpointsURL)
		endpointsResp, err := b.Client.Do(endpointsReq)
		if err != nil {
			log.Printf("[ERROR] Failed to query endpoints API for service %s: %v", service, err)
		} else {
			statusCode := endpointsResp.StatusCode
			if statusCode == http.StatusOK {
				endpointsBody, err := io.ReadAll(endpointsResp.Body)
				endpointsResp.Body.Close()
				
				if err == nil && len(endpointsBody) > 0 {
					// Try to parse the response as an array of endpoints
					var podMetricsArray []dashboard.PodMetrics
					arrayErr := json.Unmarshal(endpointsBody, &podMetricsArray)
					if arrayErr == nil && len(podMetricsArray) > 0 {
						log.Printf("[DEBUG] Parsed endpoints as array with %d items", len(podMetricsArray))
						return podMetricsArray, nil
					} else {
						log.Printf("[DEBUG] Failed to parse endpoints as array: %v", arrayErr)
					}
					
					// Try to parse the response as a single endpoint object
					var endpointsData struct {
						Pod struct {
							Name string `json:"name"`
							IP   string `json:"ip"`
							Node string `json:"node"`
						} `json:"pod"`
						Connections int `json:"connections"`
					}
					
					singleErr := json.Unmarshal(endpointsBody, &endpointsData)
					if singleErr == nil && endpointsData.Pod.Name != "" {
						podMetrics := dashboard.PodMetrics{
							PodName:           endpointsData.Pod.Name,
							PodIP:             endpointsData.Pod.IP,
							NodeName:          endpointsData.Pod.Node,
							AgentsConnections: endpointsData.Connections,
						}
						
						log.Printf("[DEBUG] Created pod metrics from endpoints API: %+v", podMetrics)
						return []dashboard.PodMetrics{podMetrics}, nil
					} else {
						log.Printf("[DEBUG] Failed to parse endpoints as single object: %v", singleErr)
					}
					
					// Try parsing the raw JSON to see what fields are present
					var rawJSON map[string]interface{}
					if err := json.Unmarshal(endpointsBody, &rawJSON); err == nil {
						log.Printf("[DEBUG] Raw JSON fields: %v", rawJSON)
					}
				}
			} else {
				// Try to read error message from non-OK responses
				defer endpointsResp.Body.Close()
				errorBody, _ := io.ReadAll(endpointsResp.Body)
				log.Printf("[ERROR] Endpoints API error response (%d): %s", statusCode, string(errorBody))
			}
		}
	}

	// Try getting all pods endpoint if available
	log.Printf("[DEBUG] Attempting to use all-pods API for service %s", service)
	allPodsURL := fmt.Sprintf("%s/pods", service)
	log.Printf("[DEBUG] All-pods URL: %s", allPodsURL)
	
	allPodsReq, err := http.NewRequest("GET", allPodsURL, nil)
	if err != nil {
		log.Printf("[ERROR] Failed to create all-pods request: %v", err)
		// Continue to next approach
	} else {
		allPodsReq.Header.Set("User-Agent", "TraefikWSBalancer/1.0")
		allPodsReq.Header.Set("Accept", "application/json")
		
		log.Printf("[DEBUG] Sending all-pods API request to %s", allPodsURL)
		allPodsResp, err := b.Client.Do(allPodsReq)
		if err != nil {
			log.Printf("[ERROR] Failed to query all-pods API for service %s: %v", service, err)
		} else {
			statusCode := allPodsResp.StatusCode
			if statusCode == http.StatusOK {
				allPodsBody, err := io.ReadAll(allPodsResp.Body)
				allPodsResp.Body.Close()
				
				if err == nil && len(allPodsBody) > 0 {
					bodyStr := string(allPodsBody)
					log.Printf("[DEBUG] All-pods API response body (%d bytes): %s", len(bodyStr), bodyStr)
					
					var podsArray []dashboard.PodMetrics
					arrayErr := json.Unmarshal(allPodsBody, &podsArray)
					if arrayErr == nil && len(podsArray) > 0 {
						log.Printf("[DEBUG] Successfully parsed all-pods as array with %d items", len(podsArray))
						return podsArray, nil
					} else {
						log.Printf("[DEBUG] Failed to parse all-pods as array: %v", arrayErr)
						
						// Try parsing as a single pod object
						var singlePod dashboard.PodMetrics
						singleErr := json.Unmarshal(allPodsBody, &singlePod)
						if singleErr == nil && singlePod.PodName != "" {
							log.Printf("[DEBUG] Successfully parsed all-pods as single pod: %+v", singlePod)
							return []dashboard.PodMetrics{singlePod}, nil
						} else {
							log.Printf("[DEBUG] Failed to parse all-pods as single pod: %v", singleErr)
						}
						
						// Try parsing the raw JSON to see what fields are present
						var rawJSON map[string]interface{}
						if err := json.Unmarshal(allPodsBody, &rawJSON); err == nil {
							log.Printf("[DEBUG] Raw JSON fields: %v", rawJSON)
						}
					}
				}
			} else {
				// Try to read error message from non-OK responses
				defer allPodsResp.Body.Close()
				errorBody, _ := io.ReadAll(allPodsResp.Body)
				log.Printf("[ERROR] All-pods API error response (%d): %s", statusCode, string(errorBody))
			}
		}
	}

	// Try to discover pods via DNS before querying the service endpoint
	dnsDiscoveredPods, found := b.discoverPodViaDNS(service)
	if found {
		log.Printf("[INFO] Successfully discovered pods via DNS for service %s", service)
		return dnsDiscoveredPods, nil
	}

	// Step 2: Fall back to querying the service for initial pod metrics.
	log.Printf("[DEBUG] Fetching connections from service %s using metric path %s", service, b.MetricPath)
	metricURL := service + b.MetricPath
	log.Printf("[DEBUG] Metric URL: %s", metricURL)
	
	metricReq, err := http.NewRequest("GET", metricURL, nil)
	if err != nil {
		log.Printf("[ERROR] Failed to create request for %s: %v", metricURL, err)
		return nil, err
	}
	
	metricReq.Header.Set("User-Agent", "TraefikWSBalancer/1.0")
	metricReq.Header.Set("Accept", "application/json")
	
	log.Printf("[DEBUG] Sending metric request to %s", metricURL)
	metricResp, err := b.Client.Do(metricReq)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch connections from service %s: %v", service, err)
		return nil, err
	}
	defer metricResp.Body.Close()
	
	log.Printf("[DEBUG] Received response from %s: status=%d", metricURL, metricResp.StatusCode)
	
	if metricResp.StatusCode != http.StatusOK {
		log.Printf("[ERROR] Non-OK status code from %s: %d", metricURL, metricResp.StatusCode)
		return nil, fmt.Errorf("service returned status %d", metricResp.StatusCode)
	}
	
	body, err := io.ReadAll(metricResp.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read response body from service %s: %v", service, err)
		return nil, err
	}
	
	log.Printf("[DEBUG] Response body from %s (%d bytes): %s", metricURL, len(body), string(body))
	
	var initialPodMetrics dashboard.PodMetrics
	podErr := json.Unmarshal(body, &initialPodMetrics)
	
	if podErr != nil {
		log.Printf("[DEBUG] Failed to decode as pod metrics (%v), trying legacy format", podErr)
		var legacyMetrics struct {
			AgentsConnections    int      `json:"agentsConnections"`
			PodName              string   `json:"podName"`
			PodIP                string   `json:"podIP"`
			NodeName             string   `json:"nodeName"`
			TotalConnections     int      `json:"totalConnectionsReceived"`
			ActiveConnections    []string `json:"activeConnections"`
		}
		legacyErr := json.Unmarshal(body, &legacyMetrics)
		if legacyErr != nil {
			log.Printf("[ERROR] Failed to decode metrics from service %s: %v", service, legacyErr)
			
			// Try parsing the raw JSON to see what fields are present
			var rawJSON map[string]interface{}
			if err := json.Unmarshal(body, &rawJSON); err == nil {
				log.Printf("[DEBUG] Raw JSON metric fields: %v", rawJSON)
				
				// Extract what we can from the raw JSON
				if agentConns, ok := rawJSON["agentsConnections"].(float64); ok {
					initialPodMetrics.AgentsConnections = int(agentConns)
				}
				if podName, ok := rawJSON["podName"].(string); ok {
					initialPodMetrics.PodName = podName
				}
				if podIP, ok := rawJSON["podIP"].(string); ok {
					initialPodMetrics.PodIP = podIP
				}
				if nodeName, ok := rawJSON["nodeName"].(string); ok {
					initialPodMetrics.NodeName = nodeName
				}
				
				log.Printf("[DEBUG] Extracted pod metrics from raw JSON: %+v", initialPodMetrics)
				return []dashboard.PodMetrics{initialPodMetrics}, nil
			}
			
			// Create a fallback dummy pod for service continuity
			dummyPod := dashboard.PodMetrics{
				PodName:           "parse-error-pod",
				PodIP:             "0.0.0.0",
				NodeName:          "parse-error-node",
				AgentsConnections: 0,
			}
			log.Printf("[WARN] Returning fallback dummy pod for parse error: %+v", dummyPod)
			return []dashboard.PodMetrics{dummyPod}, nil
		}
		
		// Use the expanded format from your Node.js server
		initialPodMetrics = dashboard.PodMetrics{
			AgentsConnections: legacyMetrics.AgentsConnections,
			PodName:           legacyMetrics.PodName,
			PodIP:             legacyMetrics.PodIP,
			NodeName:          legacyMetrics.NodeName,
		}
		
		log.Printf("[DEBUG] Parsed pod metrics from legacy format: %+v", initialPodMetrics)
	} else {
		log.Printf("[DEBUG] Parsed pod metrics from standard format: %+v", initialPodMetrics)
	}

	// If no pod info is available, return the single metric.
	if initialPodMetrics.PodName == "" || initialPodMetrics.PodIP == "" {
		log.Printf("[WARN] Pod name or IP missing from metrics response, using placeholder values")
		initialPodMetrics.PodName = initialPodMetrics.PodName
		if initialPodMetrics.PodName == "" {
			initialPodMetrics.PodName = "unnamed-pod"
		}
		initialPodMetrics.PodIP = initialPodMetrics.PodIP
		if initialPodMetrics.PodIP == "" {
			initialPodMetrics.PodIP = "0.0.0.0"
		}
		initialPodMetrics.NodeName = initialPodMetrics.NodeName
		if initialPodMetrics.NodeName == "" {
			initialPodMetrics.NodeName = "unnamed-node"
		}
	}

	log.Printf("[DEBUG] Found pod info, will attempt to discover all pods for service %s", service)
	allPodMetrics := []dashboard.PodMetrics{initialPodMetrics}

	return allPodMetrics, nil
}

// discoverPodViaDNS attempts to discover pod metrics by querying individual pod DNS names
// when K8s has dynamic DNS entries for pods
func (b *Balancer) discoverPodViaDNS(service string) ([]dashboard.PodMetrics, bool) {
	serviceURL, err := url.Parse(service)
	if err != nil {
		log.Printf("[ERROR] Failed to parse service URL: %v", err)
		return nil, false
	}
	
	// Extract the service hostname
	hostname := serviceURL.Hostname()
	if hostname == "" {
		log.Printf("[ERROR] Service hostname is empty")
		return nil, false
	}
	
	// Extract namespace and service name from hostname
	// Format is typically: service-name.namespace.svc.cluster.local
	parts := strings.Split(hostname, ".")
	if len(parts) < 2 {
		log.Printf("[ERROR] Cannot parse service hostname parts: %s", hostname)
		return nil, false
	}
	
	serviceName := parts[0]
	namespace := parts[1]
	
	// Try to find pods in the format: pod-name.service-name.namespace.svc.cluster.local
	// First, get the pod deployment prefix by trying to resolve the service
	// This is typically done with a headless service in K8s
	ips, err := net.LookupIP(hostname)
	if err != nil {
		log.Printf("[ERROR] Failed to resolve service hostname %s: %v", hostname, err)
		// Continue anyway, we'll try a different approach
	} else {
		log.Printf("[DEBUG] Service %s resolves to IPs: %v", hostname, ips)
	}
	
	// Try to discover pods via DNS from log patterns
	// In logs we see patterns like:
	// kbrain-socket-agent-65fdc7655f-aaauo.kbrain-socket-agent-ktrust-service.phaedra.svc.cluster.local
	
	// Extract unique pod identifier patterns from the hostname
	// Example: Convert "kbrain-socket-agent-ktrust-service" to find "kbrain-socket-agent-"
	prefixParts := strings.Split(serviceName, "-")
	if len(prefixParts) < 2 {
		log.Printf("[WARN] Service name '%s' doesn't follow expected pattern for DNS discovery", serviceName)
		return nil, false
	}
	
	// Take the first two parts as deployment prefix to search for
	deployPrefix := strings.Join(prefixParts[:2], "-") + "-"
	log.Printf("[DEBUG] Using deployment prefix '%s' for DNS discovery", deployPrefix)
	
	// Try multiple pod suffixes to discover active pods
	// In Kubernetes deployments, pods often have names like deployment-replicaset-podid
	// Common pod ID patterns from logs
	podSuffixes := []string{"aaauo", "aaakz", "aab22", "aaaaa", "aaaab", "aaaac", "aaaad"}
	
	var discoveredPods []dashboard.PodMetrics
	
	// Try to discover using the ReplicaSet pattern from logs
	// Find patterns like kbrain-socket-agent-65fdc7655f-aaauo
	for _, suffix := range podSuffixes {
		// Try to discover pods using the pattern from the logs
		// Format: deployPrefix-replicasetHash-podSuffix.serviceName.namespace
		
		// Look for a common replicaset hash pattern like 65fdc7655f
		// Try a few common patterns if we don't know the exact one
		replicaSetHashes := []string{"65fdc7655f", "deployment", "statefulset", ""}
		
		for _, hash := range replicaSetHashes {
			var podName string
			if hash == "" {
				// Try without replicaset hash
				podName = deployPrefix + suffix
			} else {
				podName = deployPrefix + hash + "-" + suffix
			}
			
			// Create full DNS name
			podDNS := fmt.Sprintf("%s.%s.%s.svc.cluster.local", podName, serviceName, namespace)
			log.Printf("[DEBUG] Trying to discover pod via DNS: %s%s", podDNS, b.MetricPath)
			
			// Try to query the pod directly
			podURL := fmt.Sprintf("http://%s%s", podDNS, b.MetricPath)
			req, err := http.NewRequest("GET", podURL, nil)
			if err != nil {
				continue
			}
			
			req.Header.Set("User-Agent", "TraefikWSBalancer/1.0")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			req = req.WithContext(ctx)
			
			resp, err := b.Client.Do(req)
			cancel()
			
			if err != nil {
				continue
			}
			
			if resp.StatusCode == http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				
				if err != nil {
					continue
				}
				
				var podMetrics dashboard.PodMetrics
				err = json.Unmarshal(body, &podMetrics)
				if err != nil {
					// Try legacy format
					var legacyMetrics struct {
						AgentsConnections int    `json:"agentsConnections"`
						PodName           string `json:"podName"`
						PodIP             string `json:"podIP"`
						NodeName          string `json:"nodeName"`
					}
					
					err = json.Unmarshal(body, &legacyMetrics)
					if err != nil {
						continue
					}
					
					podMetrics = dashboard.PodMetrics{
						AgentsConnections: legacyMetrics.AgentsConnections,
						PodName:           legacyMetrics.PodName,
						PodIP:             legacyMetrics.PodIP,
						NodeName:          legacyMetrics.NodeName,
					}
				}
				
				// If pod didn't return its name, use the DNS name
				if podMetrics.PodName == "" {
					podMetrics.PodName = podName
				}
				
				// If pod didn't return IP, try to resolve it
				if podMetrics.PodIP == "" {
					ips, err := net.LookupIP(podDNS)
					if err == nil && len(ips) > 0 {
						podMetrics.PodIP = ips[0].String()
					}
				}
				
				log.Printf("[INFO] Successfully discovered pod %s via DNS with %d connections", 
					podMetrics.PodName, podMetrics.AgentsConnections)
				discoveredPods = append(discoveredPods, podMetrics)
			}
		}
	}
	
	if len(discoveredPods) > 0 {
		log.Printf("[INFO] Discovered %d pods via DNS for service %s", len(discoveredPods), serviceName)
		return discoveredPods, true
	}
	
	return nil, false
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
	// Add panic recovery to prevent the balancer from crashing
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[ERROR] Recovered from panic in refreshServiceConnectionCount for %s: %v", service, r)
		}
	}()

	log.Printf("[DEBUG] Refreshing connection count for service: %s", service)
	
	podMetrics, err := b.GetAllPodsForService(service)
	if err != nil {
		log.Printf("[ERROR] Failed to get pod metrics for service %s: %v", service, err)
		// Don't return here - instead use any cached data if available
		b.MetricsMutex.RLock()
		metrics, exists := b.Metrics[service]
		b.MetricsMutex.RUnlock()
		
		if exists && len(metrics) > 0 {
			log.Printf("[INFO] Using cached metrics for service %s due to error", service)
			// Keep using the cached values
			b.CountMutex.RLock()
			count := b.ConnectionCount[service]
			b.CountMutex.RUnlock()
			return count, err
		}
		
		// If no cached data, initialize with zero to prevent "no cached count" errors
		log.Printf("[WARN] No cached metrics available for service %s, initializing with zero", service)
		
		// Update cache to avoid "no cached count" errors
		b.CountMutex.Lock()
		b.ConnectionCount[service] = 0
		b.LastRefresh[service] = time.Now()
		b.CountMutex.Unlock()
		
		// Also initialize an empty metrics map to keep things consistent
		b.MetricsMutex.Lock()
		if _, exists := b.Metrics[service]; !exists {
			b.Metrics[service] = make(map[string]dashboard.PodMetrics)
			// Add a placeholder pod for UI representation
			b.Metrics[service]["placeholder"] = dashboard.PodMetrics{
				PodName:           "no-data-available",
				PodIP:             "0.0.0.0",
				NodeName:          "unknown-node",
				AgentsConnections: 0,
			}
		}
		b.MetricsMutex.Unlock()
		
		return 0, err
	}

	if len(podMetrics) == 0 {
		log.Printf("[WARN] No pod metrics found for service %s", service)
		podMetrics = []dashboard.PodMetrics{
			{
				PodName:           "no-pods-found",
				PodIP:             "0.0.0.0",
				NodeName:          "unknown-node",
				AgentsConnections: 0,
			},
		}
	}

	// Store pod metrics in the dashboard
	b.MetricsMutex.Lock()
	metrics, exists := b.Metrics[service]
	if !exists {
		metrics = make(map[string]dashboard.PodMetrics)
		b.Metrics[service] = metrics
	} else {
		// Clear existing metrics to avoid stale data
		for k := range metrics {
			delete(metrics, k)
		}
	}

	var totalConnections int
	log.Printf("[DEBUG] Found %d pods for service %s", len(podMetrics), service)
	for _, podMetric := range podMetrics {
		if podMetric.PodName == "" {
			log.Printf("[WARN] Skipping pod with empty name for service %s", service)
			continue
		}
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

	log.Printf("[INFO] Updated connection count for service %s: %d connections across %d pods", 
		service, totalConnections, len(podMetrics))
	return totalConnections, nil
}

// Refresh periodically refreshes the connection count for all services.
func (b *Balancer) Refresh() {
	ticker := time.NewTicker(time.Duration(b.RefreshInterval) * time.Second)
	forcedFullRefresh := time.NewTicker(30 * time.Second) // Force a full refresh every 30 seconds
	forcedConnectionCheck := time.NewTicker(10 * time.Second) // Force a connection check every 10 seconds
	defer ticker.Stop()
	defer forcedFullRefresh.Stop()
	defer forcedConnectionCheck.Stop()

	// Immediately populate initial data for all services
	log.Printf("[INFO] Initial population of connection counts for all services")
	for _, service := range b.Services {
		b.refreshServiceConnectionCount(service)
	}
	
	// Keep track of consecutive failures
	failureCounts := make(map[string]int)
	maxConsecutiveFailures := 5

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
							failureCounts[s]++
							
							if failureCounts[s] >= maxConsecutiveFailures {
								log.Printf("[WARN] Service %s has failed %d consecutive times - running diagnostics", 
									s, failureCounts[s])
								go b.runDetailedDiagnostics(s)
								failureCounts[s] = 0 // Reset after diagnostics
							}
						} else {
							// Reset failure count on success
							failureCounts[s] = 0
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
		case <-forcedConnectionCheck.C:
			// Perform a basic connectivity check
			log.Printf("[DEBUG] Performing connectivity check on all services")
			for _, service := range b.Services {
				go func(s string) {
					// Create a simple HEAD request to verify connectivity
					req, err := http.NewRequest("HEAD", s, nil)
					if err != nil {
						log.Printf("[ERROR] Failed to create connectivity check request for %s: %v", s, err)
						return
					}
					
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					req = req.WithContext(ctx)
					
					resp, err := b.Client.Do(req)
					if err != nil {
						log.Printf("[ERROR] Connectivity check failed for %s: %v", s, err)
						failureCounts[s]++
						
						if failureCounts[s] >= maxConsecutiveFailures {
							log.Printf("[WARN] Service %s has failed %d connectivity checks - running diagnostics", 
								s, failureCounts[s])
							go b.runDetailedDiagnostics(s)
							failureCounts[s] = 0 // Reset after diagnostics
						}
					} else {
						resp.Body.Close()
						log.Printf("[DEBUG] Connectivity check succeeded for %s: status %d", s, resp.StatusCode)
						failureCounts[s] = 0 // Reset on success
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
		log.Printf("[ERROR] No service available for handling request to %s", req.URL.Path)
		
		// Force initialization of the first service to avoid future "no cached count" errors
		if len(b.Services) > 0 {
			log.Printf("[WARN] Force initializing cache for first service due to service selection failure")
			firstService := b.Services[0]
			
			// Ensure the ConnectionCount is initialized for this service
			b.CountMutex.Lock()
			b.ConnectionCount[firstService] = 0
			b.LastRefresh[firstService] = time.Now()
			b.CountMutex.Unlock()
			
			// Run detailed diagnostics to help troubleshoot
			go b.runDetailedDiagnostics(firstService)
			
			// Use the first service as a fallback
			selectedService = firstService
			log.Printf("[INFO] Using first service %s as fallback", selectedService)
		}
		
		// If we still don't have a service to use, return error
		if selectedService == "" {
			// Add CORS headers to the error response
			rw.Header().Set("Access-Control-Allow-Origin", "*")
			http.Error(rw, "No available backend services", http.StatusServiceUnavailable)
			return
		}
	}

	log.Printf("[INFO] Selected service %s with %d connections (lowest) for request %s. Connection counts: %v",
		selectedService,
		allServiceConnections[selectedService],
		req.URL.Path,
		allServiceConnections)

	// Handle WebSocket upgrade requests
	if isWebSocketRequest(req) {
		log.Printf("[DEBUG] Handling WebSocket request to %s", req.URL.Path)
		b.handleWebSocket(rw, req, selectedService)
		return
	}

	// Handle regular HTTP requests
	log.Printf("[DEBUG] Handling HTTP request to %s", req.URL.Path)
	b.handleHTTP(rw, req, selectedService)
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
		log.Printf("[ERROR] Failed to connect to backend WebSocket at %s: %v", targetURL, err)
		
		// Run detailed diagnostics on this service
		go b.runDetailedDiagnostics(targetService)
		
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
			// Try direct HTTP request to the service to diagnose issues
			healthCheckURL := "http" + strings.TrimPrefix(targetURL, "ws") 
			log.Printf("[DEBUG] Attempting direct HTTP request to %s", healthCheckURL)
			
			httpResp, httpErr := http.Get(healthCheckURL)
			if httpErr != nil {
				log.Printf("[ERROR] Direct HTTP request also failed: %v", httpErr)
			} else {
				defer httpResp.Body.Close()
				log.Printf("[DEBUG] Direct HTTP request succeeded with status: %d", httpResp.StatusCode)
			}
			
			rw.Header().Set("Access-Control-Allow-Origin", "*")
			http.Error(rw, "Failed to connect to backend: "+err.Error(), http.StatusBadGateway)
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

// isWebSocketRequest checks if the request is a WebSocket upgrade request
func isWebSocketRequest(req *http.Request) bool {
	// Check for the Upgrade header
	if strings.ToLower(req.Header.Get("Upgrade")) != "websocket" {
		return false
	}
	
	// Check for the Connection header containing upgrade
	connectionHeader := strings.ToLower(req.Header.Get("Connection"))
	if !strings.Contains(connectionHeader, "upgrade") {
		return false
	}
	
	// Check for the Sec-WebSocket-Key header which is required for WebSocket
	if req.Header.Get("Sec-WebSocket-Key") == "" {
		return false
	}
	
	return true
}

// Helper function to run detailed diagnostics on service connectivity issues
func (b *Balancer) runDetailedDiagnostics(service string) {
	log.Printf("[INFO] Running detailed diagnostics for service: %s", service)
	
	// Test all important endpoints to diagnose issues
	endpoints := []string{
		"/",              // Root endpoint
		"/health",        // Health check
		b.MetricPath,     // Metrics endpoint
		"/endpoints",     // Endpoints API
		"/pods",          // Pods API
		"/socket/agent",  // WebSocket endpoint
	}
	
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Don't follow redirects
		},
	}
	
	for _, endpoint := range endpoints {
		url := service
		if !strings.HasSuffix(url, "/") && !strings.HasPrefix(endpoint, "/") {
			url += "/"
		}
		url += endpoint
		
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			log.Printf("[ERROR] Diagnostic error creating request for %s: %v", url, err)
			continue
		}
		
		req.Header.Set("User-Agent", "TraefikWSBalancer/1.0")
		req.Header.Set("Accept", "application/json")
		
		log.Printf("[DEBUG] Diagnostic request to %s", url)
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[ERROR] Diagnostic failed for %s: %v", url, err)
		} else {
			body := "n/a"
			if resp.Body != nil {
				bodyBytes, _ := io.ReadAll(resp.Body)
				if len(bodyBytes) > 0 {
					if len(bodyBytes) > 500 {
						body = string(bodyBytes[:500]) + "... [truncated]"
					} else {
						body = string(bodyBytes)
					}
				}
				resp.Body.Close()
			}
			
			log.Printf("[INFO] Diagnostic result for %s: Status=%d, Headers=%v, Body=%s", 
				url, resp.StatusCode, resp.Header, body)
		}
	}
	
	// Check DNS resolution for the service hostname
	serviceURL, err := url.Parse(service)
	if err == nil && serviceURL.Hostname() != "" {
		hostname := serviceURL.Hostname()
		log.Printf("[DEBUG] Resolving hostname: %s", hostname)
		ips, err := net.LookupIP(hostname)
		if err != nil {
			log.Printf("[ERROR] DNS resolution failed for %s: %v", hostname, err)
		} else {
			log.Printf("[INFO] DNS resolution for %s: %v", hostname, ips)
		}
	}
}
