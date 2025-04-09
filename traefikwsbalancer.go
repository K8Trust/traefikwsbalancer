package traefikwsbalancer

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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
	// TraefikProviderNamespace allows specifying which namespace to use for K8s Traefik provider
	TraefikProviderNamespace string `json:"traefikProviderNamespace" yaml:"traefikProviderNamespace"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		MetricPath:              "/metric",
		BalancerMetricPath:      "/balancer-metrics",
		CacheTTL:                30, // 30 seconds default
		EnableIPScanning:        false,
		DiscoveryTimeout:        2, // 2 seconds default for discovery requests
		TraefikProviderNamespace: "",
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
	TraefikProviderNamespace string
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
		TraefikProviderNamespace: config.TraefikProviderNamespace,
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
	// Step 1: Fetch connections from initial pod through the service endpoint
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

	// If no pod info is available, return the single metric
	if initialPodMetrics.PodName == "" || initialPodMetrics.PodIP == "" {
		return []dashboard.PodMetrics{initialPodMetrics}, nil
	}

	log.Printf("[DEBUG] Found pod info, will attempt to discover all pods for service %s", service)
	allPodMetrics := []dashboard.PodMetrics{initialPodMetrics}
	discoveredPods := map[string]bool{
		initialPodMetrics.PodName: true,
	}

	// Extract service DNS name for later use
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
	namespace := "default"
	if len(serviceParts) > 1 {
		namespace = serviceParts[1]
	}
	
	// If TraefikProviderNamespace is set, use it instead
	if b.TraefikProviderNamespace != "" {
		namespace = b.TraefikProviderNamespace
	}

	// Create a dedicated discovery client with a shorter timeout
	discoveryClient := &http.Client{Timeout: b.DiscoveryTimeout}

	// Discovery Method 1: Try using direct endpoints API
	log.Printf("[DEBUG] Attempting pod discovery via service endpoints API for %s", service)
	endpointsURL := fmt.Sprintf("%s/endpoints", service)
	endpointsResp, err := discoveryClient.Get(endpointsURL)
	endpointsFound := false
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
				log.Printf("[DEBUG] Found %d pods via endpoints API", len(endpoints.Pods))
				endpointsFound = true
				for _, pod := range endpoints.Pods {
					if _, seen := discoveredPods[pod.Name]; seen {
						continue // Skip already discovered pods
					}
					
					podURL := fmt.Sprintf("http://%s%s", pod.IP, b.MetricPath)
					log.Printf("[DEBUG] Trying to contact pod %s at %s", pod.Name, podURL)
					podResp, err := discoveryClient.Get(podURL)
					if err == nil {
						podBody, err := io.ReadAll(podResp.Body)
						podResp.Body.Close()
						if err == nil {
							var podMetrics dashboard.PodMetrics
							if err := json.Unmarshal(podBody, &podMetrics); err == nil {
								if podMetrics.PodName == "" {
									podMetrics.PodName = pod.Name
								}
								if podMetrics.PodIP == "" {
									podMetrics.PodIP = pod.IP
								}
								allPodMetrics = append(allPodMetrics, podMetrics)
								discoveredPods[pod.Name] = true
								log.Printf("[DEBUG] Discovered pod %s with %d connections via endpoints API", 
									podMetrics.PodName, podMetrics.AgentsConnections)
							}
						}
					} else {
						log.Printf("[DEBUG] Failed to reach pod %s at %s: %v", pod.Name, podURL, err)
					}
				}
			}
		}
	}

	// If endpoints API didn't work, try Traefik's provider endpoint pattern
	if !endpointsFound {
		// Discovery Method 2: Try Traefik provider endpoints
		log.Printf("[DEBUG] Attempting pod discovery via Traefik provider for %s", service)
		
		// FIX: Use proper URL construction with namespace from config
		var traefikProviderURL string
		if b.TraefikProviderNamespace != "" {
			// When namespace is explicitly set in config, use it directly
			traefikProviderURL = fmt.Sprintf("http://localhost:8080/api/providers/kubernetes/services/%s/%s",
				b.TraefikProviderNamespace, headlessServiceName)
			log.Printf("[DEBUG] Using configured namespace for Traefik API: %s", b.TraefikProviderNamespace)
		} else {
			// Otherwise, use the extracted namespace from service DNS
			traefikProviderURL = fmt.Sprintf("http://localhost:8080/api/providers/kubernetes/services/%s/%s",
				namespace, headlessServiceName)
		}
		
		log.Printf("[DEBUG] Trying Traefik provider URL: %s", traefikProviderURL)
		traefikResp, err := discoveryClient.Get(traefikProviderURL)
		if err == nil {
			defer traefikResp.Body.Close()
			traefikBody, err := io.ReadAll(traefikResp.Body)
			if err == nil {
				// Traefik provider response format
				var traefikService struct {
					ServerStatus map[string]struct {
						URL string `json:"url"`
					} `json:"serverStatus"`
				}
				
				if json.Unmarshal(traefikBody, &traefikService) == nil && len(traefikService.ServerStatus) > 0 {
					log.Printf("[DEBUG] Found %d pods via Traefik provider API", len(traefikService.ServerStatus))
					for serverName, status := range traefikService.ServerStatus {
						// Extract pod IP from URL
						serverURL := status.URL
						podIP := strings.TrimPrefix(serverURL, "http://")
						podIP = strings.Split(podIP, ":")[0]
						
						if podIP == initialPodMetrics.PodIP {
							continue // Skip already discovered pod
						}
						
						podURL := fmt.Sprintf("http://%s%s", podIP, b.MetricPath)
						podResp, err := discoveryClient.Get(podURL)
						if err == nil {
							podBody, err := io.ReadAll(podResp.Body)
							podResp.Body.Close()
							if err == nil {
								var podMetrics dashboard.PodMetrics
								if err := json.Unmarshal(podBody, &podMetrics); err == nil {
									if podMetrics.PodName == "" {
										// Use server name from Traefik if pod name not available
										podMetrics.PodName = serverName
									}
									if podMetrics.PodIP == "" {
										podMetrics.PodIP = podIP
									}
									
									if _, seen := discoveredPods[podMetrics.PodName]; !seen {
										allPodMetrics = append(allPodMetrics, podMetrics)
										discoveredPods[podMetrics.PodName] = true
										log.Printf("[DEBUG] Discovered pod %s with %d connections via Traefik provider", 
											podMetrics.PodName, podMetrics.AgentsConnections)
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// Try Kubernetes API endpoint URL pattern
	if len(discoveredPods) < 2 {
		// Discovery Method 3: Try direct Kubernetes API for endpoints
		log.Printf("[DEBUG] Attempting to discover pods via direct Kubernetes endpoints API for %s", service)
		
		// Try the standard K8s API endpoint pattern
		// FIX: Use proper namespace from config
		var k8sEndpointURL string
		if b.TraefikProviderNamespace != "" {
			k8sEndpointURL = fmt.Sprintf("http://kubernetes.default.svc/api/v1/namespaces/%s/endpoints/%s", 
				b.TraefikProviderNamespace, headlessServiceName)
		} else {
			k8sEndpointURL = fmt.Sprintf("http://kubernetes.default.svc/api/v1/namespaces/%s/endpoints/%s", 
				namespace, headlessServiceName)
		}
		
		// Setup for API server authentication
		var token []byte
		tokenPath := "/var/run/secrets/kubernetes.io/serviceaccount/token"
		if _, err := os.Stat(tokenPath); err == nil {
			token, _ = os.ReadFile(tokenPath)
		}
		
		req, _ := http.NewRequest("GET", k8sEndpointURL, nil)
		if token != nil {
			req.Header.Set("Authorization", "Bearer "+string(token))
		}
		
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		k8sClient := &http.Client{Transport: tr, Timeout: b.DiscoveryTimeout}
		
		endpointResp, err := k8sClient.Do(req)
		if err == nil {
			defer endpointResp.Body.Close()
			epBody, _ := io.ReadAll(endpointResp.Body)
			
			// Parse Kubernetes endpoint response
			var endpoint struct {
				Subsets []struct {
					Addresses []struct {
						IP        string `json:"ip"`
						TargetRef struct {
							Name string `json:"name"`
						} `json:"targetRef"`
					} `json:"addresses"`
				} `json:"subsets"`
			}
			
			if json.Unmarshal(epBody, &endpoint) == nil {
				for _, subset := range endpoint.Subsets {
					for _, addr := range subset.Addresses {
						podName := addr.TargetRef.Name
						if _, seen := discoveredPods[podName]; seen {
							continue
						}
						
						podURL := fmt.Sprintf("http://%s%s", addr.IP, b.MetricPath)
						podResp, err := discoveryClient.Get(podURL)
						if err == nil {
							podBody, _ := io.ReadAll(podResp.Body)
							podResp.Body.Close()
							
							var podMetrics dashboard.PodMetrics
							if err := json.Unmarshal(podBody, &podMetrics); err == nil {
								if podMetrics.PodName == "" {
									podMetrics.PodName = podName
								}
								if podMetrics.PodIP == "" {
									podMetrics.PodIP = addr.IP
								}
								allPodMetrics = append(allPodMetrics, podMetrics)
								discoveredPods[podName] = true
								log.Printf("[DEBUG] Discovered pod %s with %d connections via K8s API", 
									podMetrics.PodName, podMetrics.AgentsConnections)
							}
						}
					}
				}
			}
		}
	}

	// Try direct pod DNS naming
	if len(discoveredPods) < 2 {
		log.Printf("[DEBUG] Attempting discovery via DNS pattern for %s", baseName)
		// Try a wider range of pod indices to discover more pods
		for i := 0; i < 20; i++ {
			potentialPodName := fmt.Sprintf("%s-%d", baseName, i)
			if _, seen := discoveredPods[potentialPodName]; seen {
				continue // Skip already discovered pods
			}
			
			// Try both direct pod name and with the domain
			// FIX: Properly use namespace configuration
			var potentialURLs []string
			
			if b.TraefikProviderNamespace != "" {
				// Use the configured namespace
				potentialURLs = []string{
					fmt.Sprintf("http://%s-%d.%s.%s.svc.cluster.local%s", 
						baseName, i, headlessServiceName, b.TraefikProviderNamespace, b.MetricPath),
					fmt.Sprintf("http://%s-%d%s", baseName, i, b.MetricPath),
				}
			} else {
				// Use the extracted namespace
				potentialURLs = []string{
					fmt.Sprintf("http://%s-%d.%s%s", baseName, i, serviceBase, b.MetricPath),
					fmt.Sprintf("http://%s-%d%s", baseName, i, b.MetricPath),
				}
			}
			
			for _, potentialURL := range potentialURLs {
				log.Printf("[DEBUG] Trying to discover pod via DNS: %s", potentialURL)
				podResp, err := discoveryClient.Get(potentialURL)
				if err != nil {
					continue
				}
				
				podBody, err := io.ReadAll(podResp.Body)
				podResp.Body.Close()
				if err != nil {
					log.Printf("[ERROR] Failed to read response from pod %s: %v", potentialURL, err)
					continue
				}
				
				var podMetrics dashboard.PodMetrics
				if err := json.Unmarshal(podBody, &podMetrics); err != nil {
					log.Printf("[ERROR] Failed to decode metrics from pod %s: %v", potentialURL, err)
					continue
				}
				
				if podMetrics.PodName == "" {
					podMetrics.PodName = potentialPodName
				}
				
				if _, seen := discoveredPods[podMetrics.PodName]; !seen {
					allPodMetrics = append(allPodMetrics, podMetrics)
					discoveredPods[podMetrics.PodName] = true
					log.Printf("[DEBUG] Discovered pod %s with %d connections via DNS",
						podMetrics.PodName, podMetrics.AgentsConnections)
				}
			}
		}
	}

	// Use IP scanning (if enabled)
	if len(discoveredPods) < 2 && initialPodMetrics.PodIP != "" && b.EnableIPScanning {
		log.Printf("[DEBUG] Attempting IP scanning for neighborhood of %s", initialPodMetrics.PodIP)
		ipParts := strings.Split(initialPodMetrics.PodIP, ".")
		if len(ipParts) == 4 {
			baseIP := strings.Join(ipParts[:3], ".")
			lastOctet, err := strconv.Atoi(ipParts[3])
			if err == nil {
				// Expand the scan range to increase chances of finding all pods
				startOctet := lastOctet - 20
				if startOctet < 1 {
					startOctet = 1
				}
				endOctet := lastOctet + 20
				if endOctet > 254 {
					endOctet = 254
				}
				
				// Create a waitgroup to parallelize IP scanning
				var wg sync.WaitGroup
				var mu sync.Mutex // To protect concurrent access to allPodMetrics
				
				// Use a semaphore to limit concurrent requests
				sem := make(chan struct{}, 10) // Allow 10 concurrent requests
				
				for i := startOctet; i <= endOctet; i++ {
					if i == lastOctet {
						continue // Skip the pod we already know
					}
					
					wg.Add(1)
					go func(octet int) {
						defer wg.Done()
						sem <- struct{}{} // Acquire semaphore
						defer func() { <-sem }() // Release semaphore
						
						potentialIP := fmt.Sprintf("%s.%d", baseIP, octet)
						potentialURL := fmt.Sprintf("http://%s%s", potentialIP, b.MetricPath)
						
						client := &http.Client{Timeout: 500 * time.Millisecond} // Short timeout for faster scanning
						podResp, err := client.Get(potentialURL)
						if err != nil {
							return
						}
						
						podBody, err := io.ReadAll(podResp.Body)
						podResp.Body.Close()
						if err != nil {
							return
						}
						
						var podMetrics dashboard.PodMetrics
						if err := json.Unmarshal(podBody, &podMetrics); err != nil {
							return
						}
						
						// If we didn't get a pod name, assign one based on IP
						if podMetrics.PodName == "" {
							podMetrics.PodName = fmt.Sprintf("pod-ip-%s", potentialIP)
						}
						
						if podMetrics.PodIP == "" {
							podMetrics.PodIP = potentialIP
						}
						
						mu.Lock()
						defer mu.Unlock()
						
						// Check if we already discovered this pod by name
						for _, existing := range allPodMetrics {
							if existing.PodName == podMetrics.PodName {
								return
							}
						}
						
						allPodMetrics = append(allPodMetrics, podMetrics)
						log.Printf("[DEBUG] Discovered pod %s with %d connections via IP scanning",
							podMetrics.PodName, podMetrics.AgentsConnections)
					}(i)
				}
				
				wg.Wait()
			}
		}
	}

	log.Printf("[DEBUG] Total pods discovered for service %s: %d", service, len(allPodMetrics))
	return allPodMetrics, nil
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
			log.Printf("[WARN] Cache miss for service %s: %v. Attempting direct fetch.", service, err)
			// Attempt to fetch directly if cache lookup fails
			connCount, _, fetchErr := b.GetConnections(service)
			if fetchErr != nil {
				log.Printf("[ERROR] Failed to fetch connections directly for service %s: %v", service, fetchErr)
				continue // Skip this service if direct fetch also fails
			}
			log.Printf("[DEBUG] Successfully fetched connections directly for service %s: %d", service, connCount)
			connections = connCount
			// Optionally store the fetched value back in the cache
			b.connCache.Store(service, connections)
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