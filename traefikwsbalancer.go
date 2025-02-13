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

// Config represents the plugin configuration
type Config struct {
	MetricPath string   `json:"metricPath,omitempty" yaml:"metricPath"`
	Pods       []string `json:"pods,omitempty" yaml:"pods"`
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
	next       http.Handler
	name       string
	pods       []string
	client     *http.Client
	metricPath string

	// Connection caching
	connCache    sync.Map
	cacheTTL     time.Duration
	lastUpdate   time.Time
	updateMutex  sync.Mutex
}

// New creates a new plugin instance
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if len(config.Pods) == 0 {
		return nil, fmt.Errorf("no pods configured")
	}

	return &Balancer{
		next:       next,
		name:       name,
		pods:       config.Pods,
		client:     &http.Client{Timeout: 5 * time.Second},
		metricPath: config.MetricPath,
		cacheTTL:   time.Duration(config.CacheTTL) * time.Second,
	}, nil
}

func (b *Balancer) getConnections(pod string) (int, error) {
	resp, err := b.client.Get(pod + b.metricPath)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var metrics struct {
		AgentsConnections int `json:"agentsConnections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
		return 0, err
	}

	return metrics.AgentsConnections, nil
}

func (b *Balancer) getCachedConnections(pod string) (int, error) {
	b.updateMutex.Lock()
	defer b.updateMutex.Unlock()

	if time.Since(b.lastUpdate) > b.cacheTTL {
		for _, p := range b.pods {
			count, err := b.getConnections(p)
			if err != nil {
				log.Printf("Error fetching connections for pod %s: %v", p, err)
				continue
			}
			b.connCache.Store(p, count)
		}
		b.lastUpdate = time.Now()
	}

	if count, ok := b.connCache.Load(pod); ok {
		return count.(int), nil
	}
	return 0, fmt.Errorf("no cached count for pod %s", pod)
}

func (b *Balancer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// Select pod with least connections
	minConnections := int(^uint(0) >> 1)
	var selectedPod string

	for _, pod := range b.pods {
		connections, err := b.getCachedConnections(pod)
		if err != nil {
			log.Printf("Error getting cached connections for pod %s: %v", pod, err)
			continue
		}

		if connections < minConnections {
			minConnections = connections
			selectedPod = pod
		}
	}

	if selectedPod == "" {
		http.Error(rw, "No available pod", http.StatusServiceUnavailable)
		return
	}

	// Create proxy request
	targetURL := selectedPod + req.URL.Path
	proxyReq, err := http.NewRequest(req.Method, targetURL, req.Body)
	if err != nil {
		http.Error(rw, "Failed to create proxy request", http.StatusInternalServerError)
		return
	}

	// Copy headers and metadata
	proxyReq.Header = req.Header.Clone()
	proxyReq.Host = req.Host
	proxyReq.URL.RawQuery = req.URL.RawQuery

	// Forward the request
	resp, err := b.client.Do(proxyReq)
	if err != nil {
		http.Error(rw, "Request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for k, v := range resp.Header {
		for _, val := range v {
			rw.Header().Add(k, val)
		}
	}

	// Write status code and body
	rw.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(rw, resp.Body); err != nil {
		log.Printf("Failed to write response body: %v", err)
	}
}