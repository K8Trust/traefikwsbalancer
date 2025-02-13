package traefikwsbalancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// ConnectionFetcher interface for getting connection counts
type ConnectionFetcher interface {
	GetConnections(string) (int, error)
}

// Config represents the plugin configuration
type Config struct {
	MetricPath string   `json:"metricPath,omitempty" yaml:"metricPath"`
	Services   []string `json:"services,omitempty" yaml:"services"`
	CacheTTL   int      `json:"cacheTTL" yaml:"cacheTTL"` // TTL in seconds
}

// CreateConfig creates the default plugin configuration
func CreateConfig() *Config {
	return &Config{
		MetricPath: "/metric",
		CacheTTL:   30, // 30 seconds default
	}
}

// Balancer is the connection balancer plugin
type Balancer struct {
	Next         http.Handler
	Name         string
	Services     []string
	Client       *http.Client
	MetricPath   string
	Fetcher      ConnectionFetcher
	DialTimeout  time.Duration
	WriteTimeout time.Duration
	ReadTimeout  time.Duration

	// Connection caching
	connCache    sync.Map
	cacheTTL     time.Duration
	lastUpdate   time.Time
	updateMutex  sync.Mutex
}

// New creates a new plugin instance
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if len(config.Services) == 0 {
		return nil, fmt.Errorf("no services configured")
	}

	log.Printf("[INFO] Creating new balancer instance with %d services", len(config.Services))
	for i, service := range config.Services {
		log.Printf("[DEBUG] Service %d: %s", i+1, service)
	}

	return &Balancer{
		Next:         next,
		Name:         name,
		Services:     config.Services,
		Client:       &http.Client{Timeout: 5 * time.Second},
		MetricPath:   config.MetricPath,
		cacheTTL:     time.Duration(config.CacheTTL) * time.Second,
		DialTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		ReadTimeout:  30 * time.Second,
	}, nil
}

// GetConnections retrieves the number of connections for a service
func (b *Balancer) GetConnections(service string) (int, error) {
	if b.Fetcher != nil {
		log.Printf("[DEBUG] Using custom connection fetcher for service %s", service)
		return b.Fetcher.GetConnections(service)
	}

	log.Printf("[DEBUG] Fetching connections from service %s using metric path %s", service, b.MetricPath)
	resp, err := b.Client.Get(service + b.MetricPath)
	if err != nil {
		log.Printf("[ERROR] Failed to fetch connections from service %s: %v", service, err)
		return 0, err
	}
	defer resp.Body.Close()

	var metrics struct {
		AgentsConnections int `json:"agentsConnections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
		log.Printf("[ERROR] Failed to decode metrics from service %s: %v", service, err)
		return 0, err
	}

	log.Printf("[DEBUG] Service %s reports %d connections", service, metrics.AgentsConnections)
	return metrics.AgentsConnections, nil
}

func (b *Balancer) getCachedConnections(service string) (int, error) {
	b.updateMutex.Lock()
	defer b.updateMutex.Unlock()

	if time.Since(b.lastUpdate) > b.cacheTTL {
		log.Printf("[DEBUG] Cache TTL expired, refreshing connection counts")
		for _, s := range b.Services {
			count, err := b.GetConnections(s)
			if err != nil {
				log.Printf("[ERROR] Error fetching connections for service %s: %v", s, err)
				continue
			}
			b.connCache.Store(s, count)
			log.Printf("[DEBUG] Updated cached connections for service %s: %d", s, count)
		}
		b.lastUpdate = time.Now()
	}

	if count, ok := b.connCache.Load(service); ok {
		return count.(int), nil
	}
	return 0, fmt.Errorf("no cached count for service %s", service)
}

func (b *Balancer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// Select service with least connections
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

	// Create proxy request
	targetURL := selectedService + req.URL.Path
	proxyReq, err := http.NewRequest(req.Method, targetURL, req.Body)
	if err != nil {
		log.Printf("[ERROR] Failed to create proxy request to %s: %v", targetURL, err)
		http.Error(rw, "Failed to create proxy request", http.StatusInternalServerError)
		return
	}

	// Copy headers and metadata
	proxyReq.Header = req.Header.Clone()
	proxyReq.Host = req.Host
	proxyReq.URL.RawQuery = req.URL.RawQuery

	// Forward the request
	log.Printf("[DEBUG] Forwarding request to %s", targetURL)
	resp, err := b.Client.Do(proxyReq)
	if err != nil {
		log.Printf("[ERROR] Request to %s failed: %v", targetURL, err)
		http.Error(rw, "Request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Success log
	log.Printf("[INFO] Successfully proxied request to %s with status %d", targetURL, resp.StatusCode)

	// Copy response headers
	for k, v := range resp.Header {
		for _, val := range v {
			rw.Header().Add(k, val)
		}
	}

	// Write status code and body
	rw.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(rw, resp.Body); err != nil {
		log.Printf("[ERROR] Failed to write response body from %s: %v", targetURL, err)
	}
}