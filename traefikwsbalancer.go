package traefikwsbalancer

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
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
	MetricPath                string   `json:"metricPath,omitempty" yaml:"metricPath"`
	BalancerMetricPath        string   `json:"balancerMetricPath,omitempty" yaml:"balancerMetricPath"`
	Services                  []string `json:"services,omitempty" yaml:"services"`
	CacheTTL                  int      `json:"cacheTTL" yaml:"cacheTTL"` // TTL in seconds
	EnableIPScanning          bool     `json:"enableIPScanning" yaml:"enableIPScanning"`
	DiscoveryTimeout          int      `json:"discoveryTimeout" yaml:"discoveryTimeout"`
	TraefikProviderNamespace  string   `json:"traefikProviderNamespace" yaml:"traefikProviderNamespace"`
	EnablePrometheusDiscovery bool     `json:"enablePrometheusDiscovery" yaml:"enablePrometheusDiscovery"`
	// TraefikMetricsURL is the URL for Traefik's own /metrics endpoint (usually http://localhost:8080/metrics)
	TraefikMetricsURL         string   `json:"traefikMetricsURL" yaml:"traefikMetricsURL"`
	DisableKubernetesDiscovery bool     `json:"disableKubernetesDiscovery" yaml:"disableKubernetesDiscovery"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		MetricPath:                "/metric",
		BalancerMetricPath:        "/balancer-metrics",
		CacheTTL:                  30,
		EnableIPScanning:          false,
		DiscoveryTimeout:          30, // Increased from 5 to 30 seconds
		TraefikProviderNamespace:  "",
		EnablePrometheusDiscovery: true, // Enable by default
		TraefikMetricsURL:         "http://localhost:8080/metrics", // Default internal Traefik metrics endpoint
		DisableKubernetesDiscovery: false,
	}
}

// PodMetrics represents metrics response from a pod.
type PodMetrics dashboard.PodMetrics

// Balancer is the connection balancer plugin.
type Balancer struct {
	Next                      http.Handler
	Name                      string
	Services                  []string
	Client                    *http.Client
	MetricPath                string
	BalancerMetricPath        string
	Fetcher                   ConnectionFetcher
	DialTimeout               time.Duration
	WriteTimeout              time.Duration
	ReadTimeout               time.Duration
	connCache                 sync.Map
	podMetrics                sync.Map // Maps service/endpoint -> []dashboard.PodMetrics
	cacheTTL                  time.Duration
	lastUpdate                time.Time
	updateMutex               sync.Mutex
	EnableIPScanning          bool
	DiscoveryTimeout          time.Duration
	TraefikProviderNamespace  string
	EnablePrometheusDiscovery bool
	TraefikMetricsURL         string // Renamed from PrometheusURL
	DisableKubernetesDiscovery bool
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

	// Ensure discovery timeout is at least 30 seconds
	discoveryTimeout := time.Duration(config.DiscoveryTimeout) * time.Second
	if discoveryTimeout < 30*time.Second {
		discoveryTimeout = 30 * time.Second
		log.Printf("[INFO] DiscoveryTimeout increased to minimum 30 seconds")
	}

	b := &Balancer{
		Next:                      next,
		Name:                      name,
		Services:                  config.Services,
		Client:                    &http.Client{Timeout: 30 * time.Second}, // Increased from 10 seconds
		MetricPath:                config.MetricPath,
		BalancerMetricPath:        config.BalancerMetricPath,
		cacheTTL:                  time.Duration(config.CacheTTL) * time.Second,
		DialTimeout:               30 * time.Second, // Increased from 10 seconds
		WriteTimeout:              30 * time.Second, // Increased from 10 seconds
		ReadTimeout:               60 * time.Second, // Increased from 30 seconds
		EnableIPScanning:          config.EnableIPScanning,
		DiscoveryTimeout:          discoveryTimeout,
		TraefikProviderNamespace:  config.TraefikProviderNamespace,
		EnablePrometheusDiscovery: config.EnablePrometheusDiscovery,
		TraefikMetricsURL:         config.TraefikMetricsURL, // Use the renamed field
		DisableKubernetesDiscovery: config.DisableKubernetesDiscovery,
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

// getServiceEndpoints queries Kubernetes API for the endpoints of a given service.
func (b *Balancer) getServiceEndpoints(service string) ([]string, error) {
	// If Kubernetes discovery is disabled, just return the service itself as the endpoint
	if b.DisableKubernetesDiscovery {
		log.Printf("[DEBUG] Kubernetes endpoint discovery disabled, using service as direct endpoint: %s", service)
		return []string{service}, nil
	}

	// Extract service name and namespace from the service URL.
	serviceBase := strings.TrimPrefix(service, "http://")
	serviceParts := strings.Split(serviceBase, ".")
	if len(serviceParts) < 2 {
		log.Printf("[WARN] Invalid service URL format: %s, using as direct endpoint", service)
		return []string{service}, nil
	}
	serviceName := serviceParts[0]
	
	// Override with configured namespace if specified.
	if b.TraefikProviderNamespace != "" {
		// namespace = b.TraefikProviderNamespace
	}

	log.Printf("[DEBUG] Fetching endpoints for service %s in namespace %s", serviceName, serviceParts[1])

	// Create Kubernetes API client with appropriate authentication.
	var token []byte
	tokenPath := "/var/run/secrets/kubernetes.io/serviceaccount/token"
	if _, err := os.Stat(tokenPath); err == nil {
		token, _ = os.ReadFile(tokenPath)
	}

	// Try HTTPS first - the secure and recommended approach
	endpoints, err := b.tryFetchEndpoints("https", serviceName, serviceParts[1], token)
	if err == nil {
		return endpoints, nil
	}
	
	log.Printf("[WARN] HTTPS endpoint discovery failed: %v. Trying HTTP fallback", err)

	// Fall back to HTTP if HTTPS fails
	endpoints, err = b.tryFetchEndpoints("http", serviceName, serviceParts[1], token)
	if err != nil {
		log.Printf("[ERROR] Both HTTPS and HTTP endpoint discovery failed. Using direct service: %v", err)
		return []string{service}, nil
	}
	
	return endpoints, nil
}

// tryFetchEndpoints attempts to fetch endpoints using the specified protocol
func (b *Balancer) tryFetchEndpoints(protocol, serviceName, namespace string, token []byte) ([]string, error) {
	k8sEndpointURL := fmt.Sprintf("%s://kubernetes.default.svc/api/v1/namespaces/%s/endpoints/%s", 
		protocol, namespace, serviceName)
	
	req, _ := http.NewRequest("GET", k8sEndpointURL, nil)
	if token != nil {
		req.Header.Set("Authorization", "Bearer "+string(token))
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialer := &net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}
			return dialer.DialContext(ctx, network, addr)
		},
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
	}
	
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	
	req = req.WithContext(ctx)
	
	k8sClient := &http.Client{
		Transport: tr,
		Timeout:   30 * time.Second,
	}

	log.Printf("[DEBUG] Sending K8s API request to %s with timeout %v", k8sEndpointURL, 30*time.Second)
	
	resp, err := k8sClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch endpoints from %s: %v", k8sEndpointURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Kubernetes API returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read endpoint response: %v", err)
	}

	// Parse Kubernetes endpoint response.
	var endpoint struct {
		Subsets []struct {
			Ports []struct {
				Port int `json:"port"`
			} `json:"ports"`
			Addresses []struct {
				IP string `json:"ip"`
			} `json:"addresses"`
		} `json:"subsets"`
	}
	
	if err := json.Unmarshal(body, &endpoint); err != nil {
		log.Printf("[ERROR] Failed to parse endpoint response: %v, body: %s", err, string(body))
		return nil, fmt.Errorf("failed to parse endpoint response: %v", err)
	}

	var endpointURLs []string
	for _, subset := range endpoint.Subsets {
		// Use the first port if available; default to port 80.
		port := 80
		if len(subset.Ports) > 0 {
			port = subset.Ports[0].Port
		}
		for _, addr := range subset.Addresses {
			endpointURL := fmt.Sprintf("http://%s:%d", addr.IP, port)
			endpointURLs = append(endpointURLs, endpointURL)
			log.Printf("[DEBUG] Found endpoint: %s", endpointURL)
		}
	}

	if len(endpointURLs) == 0 {
		log.Printf("[DEBUG] No endpoints found via API, trying direct endpoints query")
		endpointURLs = b.getDirectEndpoints(fmt.Sprintf("http://%s.%s", serviceName, namespace))
	}

	if len(endpointURLs) == 0 {
		return nil, fmt.Errorf("no endpoints found for service %s.%s", serviceName, namespace)
	}

	return endpointURLs, nil
}

// getDirectEndpoints is a fallback method for endpoint discovery.
func (b *Balancer) getDirectEndpoints(service string) []string {
	endpointsURL := fmt.Sprintf("%s/endpoints", service)
	client := &http.Client{Timeout: b.DiscoveryTimeout}

	resp, err := client.Get(endpointsURL)
	if err == nil {
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err == nil {
			var endpoints struct {
				Endpoints []string `json:"endpoints"`
			}
			if json.Unmarshal(body, &endpoints) == nil && len(endpoints.Endpoints) > 0 {
				log.Printf("[DEBUG] Found %d endpoints via direct service check", len(endpoints.Endpoints))
				return endpoints.Endpoints
			}
		}
	}
	// Fallback to using the service URL itself.
	return []string{service}
}

// GetAllPodsForService discovers all pods behind a service and fetches their metrics.
func (b *Balancer) GetAllPodsForService(service string) ([]dashboard.PodMetrics, error) {
	log.Printf("[DEBUG] Fetching initial metrics from service %s using metric path %s", service, b.MetricPath)
	client := &http.Client{Timeout: b.DiscoveryTimeout}
	resp, err := client.Get(service + b.MetricPath)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch initial metrics from service %s: %v", service, err)
		// If even the initial fetch fails, return error or empty slice
		return nil, fmt.Errorf("failed to fetch initial metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read initial response body from service %s: %v", service, err)
		return nil, fmt.Errorf("failed to read initial response: %v", err)
	}

	var initialPodMetrics dashboard.PodMetrics
	if err := json.Unmarshal(body, &initialPodMetrics); err != nil {
		log.Printf("[DEBUG] Failed to decode initial metrics as pod metrics, trying legacy format: %v", err)
		var legacyMetrics struct {
			AgentsConnections int `json:"agentsConnections"`
		}
		if err := json.Unmarshal(body, &legacyMetrics); err != nil {
			log.Printf("[ERROR] Failed to decode initial metrics from service %s (both formats): %v", service, err)
			return nil, fmt.Errorf("failed to decode initial metrics: %v", err)
		}
		
		// Create a dummy pod with the connection count for legacy format
		initialPodMetrics = dashboard.PodMetrics{
			AgentsConnections: legacyMetrics.AgentsConnections,
			PodName:           "unknown-pod-" + extractHostFromURL(service),
			PodIP:             extractHostFromURL(service),
		}
	}

	// Populate missing fields if possible
	if initialPodMetrics.PodIP == "" {
		initialPodMetrics.PodIP = extractHostFromURL(service)
	}
	if initialPodMetrics.PodName == "" {
		initialPodMetrics.PodName = "pod-" + initialPodMetrics.PodIP
	}

	log.Printf("[DEBUG] Initial pod info: Name=%s, IP=%s, Connections=%d", 
		initialPodMetrics.PodName, initialPodMetrics.PodIP, initialPodMetrics.AgentsConnections)
	
	allPodMetrics := []dashboard.PodMetrics{initialPodMetrics}
	discoveredPods := map[string]bool{
		initialPodMetrics.PodName: true,
	}

	// --- Discovery Methods --- 

	// 1. Primary Method: Traefik Prometheus Metrics Discovery
	if b.EnablePrometheusDiscovery {
		prometheusMetrics, err := b.discoverPodsViaPrometheus(service, discoveredPods)
		if err == nil && len(prometheusMetrics) > 0 {
			for _, podMetric := range prometheusMetrics {
				if _, seen := discoveredPods[podMetric.PodName]; !seen {
					allPodMetrics = append(allPodMetrics, podMetric)
					discoveredPods[podMetric.PodName] = true
				}
			}
			log.Printf("[DEBUG] Added %d pods via Traefik metrics discovery", len(prometheusMetrics))
		} else if err != nil {
			log.Printf("[WARN] Traefik metrics discovery failed: %v", err)
		}
	}

	// 2. Secondary Method: DNS Discovery (useful for headless services)
	dnsDiscoveredPods, dnsErr := b.discoverPodsViaDNS(service)
	if dnsErr == nil && len(dnsDiscoveredPods) > 0 {
		addedViaDNS := 0
		for _, podMetric := range dnsDiscoveredPods {
			if _, seen := discoveredPods[podMetric.PodName]; !seen {
				allPodMetrics = append(allPodMetrics, podMetric)
				discoveredPods[podMetric.PodName] = true
				addedViaDNS++
			}
		}
		if addedViaDNS > 0 {
			log.Printf("[DEBUG] Added %d pods via DNS discovery", addedViaDNS)
		}
	} else if dnsErr != nil {
		log.Printf("[DEBUG] DNS discovery failed or yielded no results: %v", dnsErr)
	}

	// 3. Tertiary Method: IP Scanning (if enabled)
	if b.EnableIPScanning && initialPodMetrics.PodIP != "" {
		ipScannedPods, scanErr := b.discoverPodsViaIPScan(service, initialPodMetrics.PodIP, discoveredPods)
		if scanErr == nil && len(ipScannedPods) > 0 {
			addedViaScan := 0
			for _, podMetric := range ipScannedPods {
				if _, seen := discoveredPods[podMetric.PodName]; !seen {
					allPodMetrics = append(allPodMetrics, podMetric)
					discoveredPods[podMetric.PodName] = true
					addedViaScan++
				}
			}
			if addedViaScan > 0 {
				log.Printf("[DEBUG] Added %d pods via IP scanning", addedViaScan)
			}
		} else if scanErr != nil {
			log.Printf("[DEBUG] IP scanning failed: %v", scanErr)
		}
	}

	log.Printf("[INFO] Total unique pods discovered for service %s: %d", service, len(allPodMetrics))
	return allPodMetrics, nil
}

// Helper function to extract host from URL
func extractHostFromURL(url string) string {
	url = strings.TrimPrefix(url, "http://")
	url = strings.TrimPrefix(url, "https://")
	hostPort := strings.Split(url, ":")
	host := strings.Split(hostPort[0], "/")[0]
	return host
}

// discoverPodsViaDNS attempts to discover pods by resolving DNS SRV records for headless services
func (b *Balancer) discoverPodsViaDNS(service string) ([]dashboard.PodMetrics, error) {
	// Extract service name and namespace from the service URL
	serviceBase := strings.TrimPrefix(service, "http://")
	serviceBase = strings.TrimPrefix(serviceBase, "https://")
	serviceParts := strings.Split(serviceBase, ".")
	if len(serviceParts) < 2 {
		return nil, fmt.Errorf("invalid service URL format")
	}
	
	serviceName := serviceParts[0]
	
	// Try to resolve IPs directly using DNS lookups
	// This works for both headless and normal services
	ips, err := net.LookupHost(serviceBase)
	if err != nil {
		return nil, fmt.Errorf("DNS lookup failed: %v", err)
	}
	
	var allPodMetrics []dashboard.PodMetrics
	seenIPs := make(map[string]bool)
	
	// For each IP, try to fetch metrics
	for _, ip := range ips {
		if seenIPs[ip] {
			continue
		}
		seenIPs[ip] = true
		
		podURL := fmt.Sprintf("http://%s%s", ip, b.MetricPath)
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(podURL)
		if err != nil {
			log.Printf("[DEBUG] Failed to connect to pod IP %s: %v", ip, err)
			continue
		}
		
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("[DEBUG] Failed to read response from pod IP %s: %v", ip, err)
			continue
		}
		
		var podMetrics dashboard.PodMetrics
		if err := json.Unmarshal(body, &podMetrics); err != nil {
			// Try legacy format
			var legacyMetrics struct {
				AgentsConnections int `json:"agentsConnections"`
			}
			if err := json.Unmarshal(body, &legacyMetrics); err != nil {
				log.Printf("[DEBUG] Failed to parse metrics from pod IP %s: %v", ip, err)
				continue
			}
			podMetrics = dashboard.PodMetrics{
				AgentsConnections: legacyMetrics.AgentsConnections,
				PodName:           fmt.Sprintf("%s-pod-%s", serviceName, ip),
				PodIP:             ip,
			}
		}
		
		// Ensure we have proper pod name and IP
		if podMetrics.PodIP == "" {
			podMetrics.PodIP = ip
		}
		if podMetrics.PodName == "" {
			podMetrics.PodName = fmt.Sprintf("%s-pod-%s", serviceName, ip)
		}
		
		allPodMetrics = append(allPodMetrics, podMetrics)
		log.Printf("[DEBUG] Discovered pod %s (%s) via DNS with %d connections", 
			podMetrics.PodName, podMetrics.PodIP, podMetrics.AgentsConnections)
	}
	
	return allPodMetrics, nil
}

// discoverPodsViaIPScan attempts to discover pods in the same subnet by scanning IP addresses
func (b *Balancer) discoverPodsViaIPScan(service string, knownPodIP string, discoveredPods map[string]bool) ([]dashboard.PodMetrics, error) {
	if knownPodIP == "" {
		return nil, fmt.Errorf("no known pod IP to base scan on")
	}
	
	// Extract the network prefix (first 3 octets) to scan the subnet
	ipParts := strings.Split(knownPodIP, ".")
	if len(ipParts) != 4 {
		return nil, fmt.Errorf("invalid IP format: %s", knownPodIP)
	}
	
	networkPrefix := fmt.Sprintf("%s.%s.%s", ipParts[0], ipParts[1], ipParts[2])
	lastOctet, err := strconv.Atoi(ipParts[3])
	if err != nil {
		return nil, fmt.Errorf("invalid IP format: %s", knownPodIP)
	}
	
	// Define scan range (adjust based on subnet size)
	scanStart := lastOctet - 10
	if scanStart < 1 {
		scanStart = 1
	}
	scanEnd := lastOctet + 10
	if scanEnd > 254 {
		scanEnd = 254
	}
	
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allPodMetrics []dashboard.PodMetrics
	
	// Use a semaphore to limit concurrent scans
	sem := make(chan struct{}, 10)
	
	for i := scanStart; i <= scanEnd; i++ {
		if i == lastOctet {
			continue // Skip the known IP
		}
		
		ipToScan := fmt.Sprintf("%s.%d", networkPrefix, i)
		wg.Add(1)
		sem <- struct{}{}
		
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			
			podURL := fmt.Sprintf("http://%s%s", ip, b.MetricPath)
			client := &http.Client{Timeout: 2 * time.Second} // Short timeout for scanning
			resp, err := client.Get(podURL)
			if err != nil {
				return // Silently fail, IP probably not in use
			}
			defer resp.Body.Close()
			
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return
			}
			
			var podMetrics dashboard.PodMetrics
			if err := json.Unmarshal(body, &podMetrics); err != nil {
				// Try legacy format
				var legacyMetrics struct {
					AgentsConnections int `json:"agentsConnections"`
				}
				if err := json.Unmarshal(body, &legacyMetrics); err != nil {
					return
				}
				podName := fmt.Sprintf("scanned-pod-%s", ip)
				podMetrics = dashboard.PodMetrics{
					AgentsConnections: legacyMetrics.AgentsConnections,
					PodName:           podName,
					PodIP:             ip,
				}
			}
			
			// Ensure we have pod name and IP
			if podMetrics.PodIP == "" {
				podMetrics.PodIP = ip
			}
			if podMetrics.PodName == "" {
				podMetrics.PodName = fmt.Sprintf("scanned-pod-%s", ip)
			}
			
			mu.Lock()
			defer mu.Unlock()
			
			if _, seen := discoveredPods[podMetrics.PodName]; !seen {
				allPodMetrics = append(allPodMetrics, podMetrics)
				log.Printf("[DEBUG] IP scan discovered pod %s (%s) with %d connections", 
					podMetrics.PodName, podMetrics.PodIP, podMetrics.AgentsConnections)
			}
		}(ipToScan)
	}
	
	wg.Wait()
	return allPodMetrics, nil
}

// discoverPodsViaPrometheus attempts to discover pods by querying Traefik's Prometheus metrics endpoint.
func (b *Balancer) discoverPodsViaPrometheus(service string, discoveredPods map[string]bool) ([]dashboard.PodMetrics, error) {
	if !b.EnablePrometheusDiscovery {
		return nil, fmt.Errorf("Prometheus discovery disabled")
	}

	log.Printf("[DEBUG] Attempting pod discovery via Traefik metrics endpoint: %s for service %s", b.TraefikMetricsURL, service)
	serviceBase := strings.TrimPrefix(service, "http://")
	serviceBase = strings.TrimPrefix(serviceBase, "https://") // Handle https as well
	serviceParts := strings.Split(serviceBase, ".")
	if len(serviceParts) == 0 {
		return nil, fmt.Errorf("invalid service URL format")
	}
	headlessServiceName := serviceParts[0] // Assuming the first part is the k8s service name
	metricsURL := b.TraefikMetricsURL
	client := &http.Client{Timeout: b.DiscoveryTimeout}

	resp, err := client.Get(metricsURL)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch Traefik metrics from %s: %v", metricsURL, err)
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[ERROR] Traefik metrics endpoint %s returned non-OK status: %d", metricsURL, resp.StatusCode)
		return nil, fmt.Errorf("metrics endpoint %s returned status %d", metricsURL, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to read Traefik metrics from %s: %v", metricsURL, err)
		return nil, err
	}

	metrics := string(body)
	podMetricsMap := make(map[string]dashboard.PodMetrics)
	var allPodMetrics []dashboard.PodMetrics
	lines := strings.Split(metrics, "\n")

	// Example Traefik metric line: traefik_service_server_up{server="http://10.42.1.23:80",service="my-service@kubernetes"} 1
	// We need to extract the server IP from lines matching the target service.
	
	// Construct the pattern to match the service label
	// Adjust this pattern based on how Traefik names your Kubernetes services (CRD vs Ingress)
	// For KubernetesCRD provider, it's often serviceName-namespace@kubernetes
	// For KubernetesIngress provider, it might be different. Let's try a general pattern.
	// Note: Need to be careful with headless service names vs service URL hostname.
	targetServiceLabel := fmt.Sprintf("service=\"%s\", headlessServiceName", headlessServiceName) // Match start of service label
	
	serverPattern := regexp.MustCompile(`server=\"http://(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}):\d+\"`) // Extracts IP

	for _, line := range lines {
		// Check if the line contains the target service label and is a relevant metric (e.g., server_up)
		if strings.Contains(line, targetServiceLabel) && strings.Contains(line, "traefik_service_server_up") {
			matches := serverPattern.FindStringSubmatch(line)
			if len(matches) > 1 {
				podIP := matches[1]
				
				// Now fetch the actual connection metrics from the discovered pod IP
				podMetrics, err := b.fetchPodMetrics(podIP)
				if err != nil {
					log.Printf("[DEBUG] Failed to get metrics from discovered pod %s: %v", podIP, err)
					continue
				}
				
				// Ensure pod name is unique if not provided by metric endpoint
				if podMetrics.PodName == "" {
					podMetrics.PodName = fmt.Sprintf("prom-%s-%s", headlessServiceName, podIP)
				}
				if podMetrics.PodIP == "" {
					podMetrics.PodIP = podIP
				}

				mu := sync.Mutex{}
				mu.Lock()
				if _, exists := podMetricsMap[podMetrics.PodName]; !exists {
					if _, seen := discoveredPods[podMetrics.PodName]; !seen {
						podMetricsMap[podMetrics.PodName] = podMetrics
						log.Printf("[DEBUG] Discovered pod %s (%s) via Traefik metrics with %d connections", podMetrics.PodName, podMetrics.PodIP, podMetrics.AgentsConnections)
					}
				}
				mu.Unlock()
			}
		}
	}

	mu := sync.Mutex{}
	mu.Lock()
	for _, podMetrics := range podMetricsMap {
		allPodMetrics = append(allPodMetrics, podMetrics)
		discoveredPods[podMetrics.PodName] = true // Mark as discovered
	}
	mu.Unlock()

	log.Printf("[DEBUG] Found %d unique, previously undiscovered pods via Traefik metrics", len(allPodMetrics))
	return allPodMetrics, nil
}

// fetchPodMetrics fetches and parses metrics from a specific pod IP.
func (b *Balancer) fetchPodMetrics(podIP string) (dashboard.PodMetrics, error) {
	podURL := fmt.Sprintf("http://%s%s", podIP, b.MetricPath)
	client := &http.Client{Timeout: 5 * time.Second} // Shorter timeout for individual pod fetch

	resp, err := client.Get(podURL)
	if err != nil {
		return dashboard.PodMetrics{}, fmt.Errorf("failed to connect to pod %s: %v", podIP, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return dashboard.PodMetrics{}, fmt.Errorf("pod %s returned status %d", podIP, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return dashboard.PodMetrics{}, fmt.Errorf("failed to read response from pod %s: %v", podIP, err)
	}

	var podMetrics dashboard.PodMetrics
	if err := json.Unmarshal(body, &podMetrics); err != nil {
		// Try legacy format
		var legacyMetrics struct {
			AgentsConnections int `json:"agentsConnections"`
		}
		if err := json.Unmarshal(body, &legacyMetrics); err != nil {
			return dashboard.PodMetrics{}, fmt.Errorf("failed to parse metrics from pod %s: %v", podIP, err)
		}
		// Create metrics struct for legacy format
		podMetrics = dashboard.PodMetrics{
			AgentsConnections: legacyMetrics.AgentsConnections,
			// PodName and PodIP will be filled in by the caller if missing
		}
	}

	return podMetrics, nil
}

// getCachedConnections returns the cached connection count for a service or endpoint.
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
		ticker := time.NewTicker(b.cacheTTL / 10)
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

// refreshServiceConnectionCount refreshes cached connection counts for a single service using endpoint discovery.
func (b *Balancer) refreshServiceConnectionCount(service string) {
	b.updateMutex.Lock()
	defer b.updateMutex.Unlock()

	log.Printf("[DEBUG] Refreshing connection counts for service %s in background", service)
	
	// Get endpoints via Kubernetes API.
	endpoints, err := b.getServiceEndpoints(service)
	if err != nil {
		// This is now redundant since getServiceEndpoints will return the service as a fallback
		// But we keep it for clarity and compatibility
		log.Printf("[DEBUG] Using default fallback due to endpoint discovery error: %v", err)
		
		// Fallback: use legacy pod discovery.
		allPodMetrics, err := b.GetAllPodsForService(service)
		if err != nil {
			log.Printf("[DEBUG] Error fetching pod metrics for service %s: %v", service, err)
			return
		}
		totalConnections := 0
		for _, pod := range allPodMetrics {
			totalConnections += pod.AgentsConnections
		}
		b.connCache.Store(service, totalConnections)
		b.podMetrics.Store(service, allPodMetrics)
		b.lastUpdate = time.Now()
		log.Printf("[DEBUG] Updated cached connections for service %s: %d connections across %d pods", service, totalConnections, len(allPodMetrics))
		return
	}

	var allEndpointMetrics []dashboard.PodMetrics
	totalConnections := 0

	// Track endpoint fetch failures
	fetchFailures := 0
	totalEndpoints := len(endpoints)

	for _, endpoint := range endpoints {
		allPodMetrics, err := b.GetAllPodsForService(endpoint)
		if err != nil {
			log.Printf("[DEBUG] Error fetching pod metrics for endpoint %s: %v", endpoint, err)
			fetchFailures++
			continue
		}
		b.podMetrics.Store(endpoint, allPodMetrics)
		endpointConnections := 0
		for _, pod := range allPodMetrics {
			endpointConnections += pod.AgentsConnections
			allEndpointMetrics = append(allEndpointMetrics, pod)
		}
		b.connCache.Store(endpoint, endpointConnections)
		totalConnections += endpointConnections
		log.Printf("[DEBUG] Updated cached connections for endpoint %s: %d connections across %d pods", endpoint, endpointConnections, len(allPodMetrics))
	}

	// Only log a warning if all endpoint fetches failed
	if fetchFailures > 0 && fetchFailures == totalEndpoints {
		log.Printf("[WARN] All endpoint fetches failed (%d/%d) for service %s", fetchFailures, totalEndpoints, service)
	}

	// Aggregate totals under the service key.
	b.connCache.Store(service, totalConnections)
	b.podMetrics.Store(service, allEndpointMetrics)
	b.lastUpdate = time.Now()
	log.Printf("[DEBUG] Updated cached connections for service %s: %d connections across %d endpoints", service, totalConnections, len(endpoints))
}

// ServeHTTP handles incoming HTTP requests by selecting the endpoint with the fewest active connections.
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
	var selectedEndpoint string

	log.Printf("[DEBUG] Request received: %s %s", req.Method, req.URL.Path)
	log.Printf("[DEBUG] Checking connection counts across services and endpoints:")
	allEndpointConnections := make(map[string]int)

	for _, service := range b.Services {
		endpoints, err := b.getServiceEndpoints(service)
		if err != nil {
			log.Printf("[WARN] Failed to get endpoints for service %s: %v", service, err)
			// Fallback to the service directly.
			connections, err := b.getCachedConnections(service)
			if err != nil {
				log.Printf("[WARN] Cache miss for service %s: %v. Attempting direct fetch.", service, err)
				connCount, _, fetchErr := b.GetConnections(service)
				if fetchErr != nil {
					log.Printf("[ERROR] Failed to fetch connections directly for service %s: %v", service, fetchErr)
					continue
				}
				log.Printf("[DEBUG] Fetched connections directly for service %s: %d", service, connCount)
				connections = connCount
				b.connCache.Store(service, connections)
			}
			allEndpointConnections[service] = connections
			log.Printf("[DEBUG] Service %s has %d active connections", service, connections)
			if connections < minConnections {
				minConnections = connections
				selectedEndpoint = service
				log.Printf("[DEBUG] New minimum found: service %s with %d connections", service, connections)
			}
			continue
		}

		// Iterate over each discovered endpoint.
		for _, endpoint := range endpoints {
			connections, err := b.getCachedConnections(endpoint)
			if err != nil {
				log.Printf("[DEBUG] Cache miss for endpoint %s. Attempting direct fetch.", endpoint)
				connCount, _, fetchErr := b.GetConnections(endpoint)
				if fetchErr != nil {
					log.Printf("[ERROR] Failed to fetch connections for endpoint %s: %v", endpoint, fetchErr)
					continue
				}
				log.Printf("[DEBUG] Fetched connections for endpoint %s: %d", endpoint, connCount)
				connections = connCount
				b.connCache.Store(endpoint, connections)
			}
			allEndpointConnections[endpoint] = connections
			log.Printf("[DEBUG] Endpoint %s has %d active connections", endpoint, connections)
			if connections < minConnections {
				minConnections = connections
				selectedEndpoint = endpoint
				log.Printf("[DEBUG] New minimum found: endpoint %s with %d connections", endpoint, connections)
			}
		}
	}

	if selectedEndpoint == "" {
		log.Printf("[ERROR] No available endpoints found")
		http.Error(rw, "No available service", http.StatusServiceUnavailable)
		return
	}

	log.Printf("[INFO] Selected endpoint %s with %d connections (lowest) for request %s. Connection counts: %v",
		selectedEndpoint,
		allEndpointConnections[selectedEndpoint],
		req.URL.Path,
		allEndpointConnections)

	if ws.IsWebSocketUpgrade(req) {
		log.Printf("[DEBUG] Handling WebSocket upgrade request")
		b.handleWebSocket(rw, req, selectedEndpoint)
		return
	}

	b.handleHTTP(rw, req, selectedEndpoint)
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