package traefikwsbalancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/K8Trust/traefikwsbalancer/internal/dashboard"
)

// PrometheusConnectionFetcher implements ConnectionFetcher interface using Prometheus API
type PrometheusConnectionFetcher struct {
	Client           *http.Client
	PrometheusURL    string
	MetricName       string
	ServiceLabelName string
	PodLabelName     string
	Timeout          time.Duration
}

type prometheusVectorResult struct {
	Metric map[string]string `json:"metric"`
	Value  []interface{}     `json:"value"` // [timestamp, value]
}

type prometheusQueryResult struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string                  `json:"resultType"`
		Result     []prometheusVectorResult `json:"result"`
	} `json:"data"`
}

// NewPrometheusConnectionFetcher creates a new connection fetcher that uses Prometheus
func NewPrometheusConnectionFetcher(prometheusURL, metricName, serviceLabelName, podLabelName string, timeout time.Duration) *PrometheusConnectionFetcher {
	return &PrometheusConnectionFetcher{
		Client:           &http.Client{Timeout: timeout},
		PrometheusURL:    strings.TrimSuffix(prometheusURL, "/"),
		MetricName:       metricName,
		ServiceLabelName: serviceLabelName,
		PodLabelName:     podLabelName,
		Timeout:          timeout,
	}
}

// GetConnections implements ConnectionFetcher interface by querying Prometheus
func (p *PrometheusConnectionFetcher) GetConnections(serviceURL string) (int, error) {
	// Extract service name from URL
	serviceName := extractServiceName(serviceURL)
	if serviceName == "" {
		return 0, fmt.Errorf("failed to extract service name from URL: %s", serviceURL)
	}

	log.Printf("[DEBUG] Querying Prometheus for service %s connections using metric %s", serviceName, p.MetricName)
	
	// Build query to get connection counts for all pods in this service
	query := fmt.Sprintf("%s{%s=\"%s\"}", p.MetricName, p.ServiceLabelName, serviceName)
	podMetrics, err := p.queryPrometheus(query)
	if err != nil {
		return 0, fmt.Errorf("prometheus query failed: %v", err)
	}

	// Calculate total connections across all pods
	totalConnections := 0
	for _, pod := range podMetrics {
		totalConnections += pod.AgentsConnections
	}

	return totalConnections, nil
}

// GetAllPodsForService queries Prometheus to get metrics for each pod in a service
func (p *PrometheusConnectionFetcher) GetAllPodsForService(serviceURL string) ([]dashboard.PodMetrics, error) {
	// Extract service name from URL
	serviceName := extractServiceName(serviceURL)
	if serviceName == "" {
		return nil, fmt.Errorf("failed to extract service name from URL: %s", serviceURL)
	}

	// Query Prometheus for all pods in this service
	query := fmt.Sprintf("%s{%s=\"%s\"}", p.MetricName, p.ServiceLabelName, serviceName)
	return p.queryPrometheus(query)
}

// queryPrometheus executes a PromQL query and returns pod metrics
func (p *PrometheusConnectionFetcher) queryPrometheus(query string) ([]dashboard.PodMetrics, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.Timeout)
	defer cancel()

	// Prepare API URL with query
	apiURL := fmt.Sprintf("%s/api/v1/query", p.PrometheusURL)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}

	// Add query parameters
	q := req.URL.Query()
	q.Add("query", query)
	req.URL.RawQuery = q.Encode()

	// Execute request
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Read and parse response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result prometheusQueryResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	// Check for success status
	if result.Status != "success" {
		return nil, fmt.Errorf("prometheus returned non-success status: %s", result.Status)
	}

	// Convert Prometheus results to PodMetrics
	metrics := make([]dashboard.PodMetrics, 0, len(result.Data.Result))
	for _, item := range result.Data.Result {
		podName := item.Metric[p.PodLabelName]
		
		// Extract node name if present
		nodeName := item.Metric["node"]
		
		// Extract pod IP if present
		podIP := item.Metric["pod_ip"]
		
		// Parse the connection count value
		var connectionCount float64
		if len(item.Value) >= 2 {
			switch v := item.Value[1].(type) {
			case string:
				if count, err := parseFloat(v); err == nil {
					connectionCount = count
				}
			case float64:
				connectionCount = v
			}
		}

		metrics = append(metrics, dashboard.PodMetrics{
			PodName:           podName,
			AgentsConnections: int(connectionCount),
			PodIP:             podIP,
			NodeName:          nodeName,
		})
	}

	return metrics, nil
}

// Helper function to extract service name from a URL
func extractServiceName(serviceURL string) string {
	// Parse URL to extract hostname
	u, err := url.Parse(serviceURL)
	if err != nil {
		log.Printf("[ERROR] Failed to parse service URL %s: %v", serviceURL, err)
		return ""
	}

	// Extract hostname
	hostname := u.Hostname()
	
	// If it's a K8s service URL, it might be in the format:
	// service-name.namespace.svc.cluster.local
	// We want to extract just the service name
	parts := strings.Split(hostname, ".")
	if len(parts) > 0 {
		return parts[0]
	}
	
	return hostname
}

// Helper to parse float from string
func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	if err != nil {
		return 0, err
	}
	return f, nil
}