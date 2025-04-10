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

	// Configuration options
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

	// Discovery configuration.
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

// ParseServiceInfo extracts service information from the URL
func ParseServiceInfo(serviceURL string) (string, string, string) {
	// Remove http:// prefix if present
	serviceURL = strings.TrimPrefix(serviceURL, "http://")
	
	// Extract port if present
	port := "80"
	hostPart := serviceURL
	
	if idx := strings.Index(serviceURL, "/"); idx != -1 {
		hostPart = serviceURL[:idx]
	}
	
	if idx := strings.Index(hostPart, ":"); idx != -1 {
		port = hostPart[idx+1:]
		hostPart = hostPart[:idx]
	}
	
	// Split by dots to get service name and namespace
	parts := strings.Split(hostPart, ".")
	serviceName := parts[0]
	namespace := "default"
	
	if len(parts) > 1 {
		namespace = parts[1]
	}
	
	return serviceName, namespace, port
}

// GetAllPodsForService discovers all pods behind a service and fetches their metrics
// This function improves pod discovery for the WebSocket balancer

func (b *Balancer) GetAllPodsForService(service string) ([]dashboard.PodMetrics, error) {
    // Step 1: Query the service for initial pod metrics
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

    // Extract service DNS name and pod naming pattern
    serviceBase := strings.TrimPrefix(service, "http://")
    podName := initialPodMetrics.PodName

    // Extract deployment name from pod name (remove the random suffix)
    // For example, from "kbrain-socket-agent-69b6f484c7-bk4nk", get "kbrain-socket-agent-69b6f484c7"
    lastDashIndex := strings.LastIndex(podName, "-")
    if lastDashIndex == -1 {
        return allPodMetrics, nil
    }
    
    deploymentPrefix := podName[:lastDashIndex]
    log.Printf("[DEBUG] Extracted deployment prefix: %s", deploymentPrefix)

    // Create a dedicated discovery client with a shorter timeout
    discoveryClient := &http.Client{Timeout: b.DiscoveryTimeout}

    // STRATEGY 1: Try using the endpoints API if available
    endpointsURL := fmt.Sprintf("%s/endpoints", service)
    log.Printf("[DEBUG] Trying to discover pods via endpoints API: %s", endpointsURL)
    endpointsResp, err := discoveryClient.Get(endpointsURL)
    if err == nil {
        // Process endpoints response
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

    // STRATEGY 2: Try direct DNS for each pod in the deployment
    // Generate common patterns for pod naming with the deployment prefix
    log.Printf("[DEBUG] Trying DNS discovery for pods with prefix %s", deploymentPrefix)
    
    // Try all alphanumeric suffixes (common patterns in Kubernetes)
    // This covers patterns like: deployment-69b6f484c7-xxxxx where xxxxx is the random suffix
    suffixes := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", 
                       "n", "o", "p", "q", "r", "s", "t", "u", "v", "w", "x", "y", "z",
                       "0", "1", "2", "3", "4", "5", "6", "7", "8", "9"}
    
    for _, firstChar := range suffixes {
        for _, secondChar := range suffixes {
            for _, thirdChar := range suffixes {
                for _, fourthChar := range suffixes {
                    for _, fifthChar := range suffixes {
                        potentialSuffix := firstChar + secondChar + thirdChar + fourthChar + fifthChar
                        potentialPodName := fmt.Sprintf("%s-%s", deploymentPrefix, potentialSuffix)
                        
                        // Skip the pod we already know about
                        if potentialPodName == initialPodMetrics.PodName {
                            continue
                        }
                        
                        // Don't try more than 20 possible pod names to avoid excessive requests
                        if len(allPodMetrics) > 20 {
                            return allPodMetrics, nil
                        }
                        
                        // Try direct pod connection
                        directPodURL := fmt.Sprintf("http://%s.%s%s", potentialPodName, serviceBase, b.MetricPath)
                        log.Printf("[DEBUG] Trying to discover pod via DNS: %s", directPodURL)
                        podResp, err := discoveryClient.Get(directPodURL)
                        if err != nil {
                            continue
                        }
                        
                        // Successfully connected to a pod
                        podBody, err := io.ReadAll(podResp.Body)
                        podResp.Body.Close()
                        if err != nil {
                            log.Printf("[ERROR] Failed to read response from pod %s: %v", directPodURL, err)
                            continue
                        }
                        
                        var podMetrics dashboard.PodMetrics
                        if err := json.Unmarshal(podBody, &podMetrics); err != nil {
                            log.Printf("[ERROR] Failed to decode metrics from pod %s: %v", directPodURL, err)
                            continue
                        }
                        
                        // Skip if we found a duplicate pod somehow
                        if podMetrics.PodName == initialPodMetrics.PodName {
                            continue
                        }
                        
                        // Add the discovered pod
                        allPodMetrics = append(allPodMetrics, podMetrics)
                        log.Printf("[DEBUG] Discovered pod %s with %d connections via DNS",
                            podMetrics.PodName, podMetrics.AgentsConnections)
                        
                        // Exit early if we found a second pod
                        if len(allPodMetrics) >= 2 && potentialPodName != podName {
                            break
                        }
                    }
                    if len(allPodMetrics) >= 2 {
                        break
                    }
                }
                if len(allPodMetrics) >= 2 {
                    break
                }
            }
            if len(allPodMetrics) >= 2 {
                break
            }
        }
        if len(allPodMetrics) >= 2 {
            break
        }
    }

    // STRATEGY 3: Try IP-based discovery (if enabled)
    if initialPodMetrics.PodIP != "" && b.EnableIPScanning {
        // Extract base IP network (e.g., "100.68.35" from "100.68.35.10")
        ipParts := strings.Split(initialPodMetrics.PodIP, ".")
        if len(ipParts) == 4 {
            baseIP := strings.Join(ipParts[:3], ".")
            log.Printf("[DEBUG] Trying IP scanning in network %s.*", baseIP)
            
            // Try scanning the subnet for other pods
            lastOctet, err := strconv.Atoi(ipParts[3])
            if err == nil {
                // Scan a range around the known pod IP
                for i := 1; i <= 254; i++ {
                    // Don't try the IP we already know
                    if i == lastOctet {
                        continue
                    }
                    
                    // Limit the number of IP probes to avoid excessive requests
                    if len(allPodMetrics) > 10 || i > 30 {
                        break
                    }
                    
                    // Try this IP address
                    potentialIP := fmt.Sprintf("%s.%d", baseIP, i)
                    potentialURL := fmt.Sprintf("http://%s%s", potentialIP, b.MetricPath)
                    log.Printf("[DEBUG] Trying IP scan at %s", potentialURL)
                    
                    podResp, err := discoveryClient.Get(potentialURL)
                    if err != nil {
                        continue
                    }
                    
                    // Successfully connected to a pod
                    podBody, _ := io.ReadAll(podResp.Body)
                    podResp.Body.Close()
                    
                    var podMetrics dashboard.PodMetrics
                    if json.Unmarshal(podBody, &podMetrics) != nil {
                        continue
                    }
                    
                    // Skip if podName is empty or matches the initial pod
                    if podMetrics.PodName == "" || podMetrics.PodName == initialPodMetrics.PodName {
                        continue
                    }
                    
                    // Add this pod to our list
                    allPodMetrics = append(allPodMetrics, podMetrics)
                    log.Printf("[DEBUG] Discovered pod %s with %d connections via IP scanning",
                        podMetrics.PodName, podMetrics.AgentsConnections)
                }
            }
        }
    }

    // Log the final count of discovered pods
    log.Printf("[INFO] Found a total of %d pods for service %s", len(allPodMetrics), service)
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