package dashboard

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ServiceMetric represents metrics for a service
type ServiceMetric struct {
	URL         string `json:"url"`
	Connections int    `json:"connections"`
}

// PodMetrics represents metrics for a pod
type PodMetrics struct {
	AgentsConnections int    `json:"agentsConnections"`
	PodName           string `json:"podName,omitempty"`
	PodIP             string `json:"podIP,omitempty"`
	NodeName          string `json:"nodeName,omitempty"`
}

// MetricsData contains all the metrics information needed for rendering
type MetricsData struct {
	Timestamp          string
	Services           []ServiceMetric
	PodMetricsMap      map[string][]PodMetrics
	TotalConnections   int
	ServiceCount       int
	PodCount           int
	RefreshInterval    int
	ForcedRefreshInterval int
	BalancerMetricPath string
	LastRefresh        map[string]time.Time
}

// RenderHTML renders the HTML dashboard with the provided metrics data
func RenderHTML(rw http.ResponseWriter, req *http.Request, data MetricsData) {
	log.Printf("[DEBUG] Rendering HTML dashboard")
	
	// Get the refresh interval from query parameter or use default
	refreshIntervalStr := req.URL.Query().Get("refreshInterval")
	if interval, err := strconv.Atoi(refreshIntervalStr); err == nil && interval > 0 {
		data.RefreshInterval = interval
	} else if data.RefreshInterval == 0 {
		data.RefreshInterval = 10 // Default to 10 seconds if not specified
	}
	
	// Generate HTML
	html := getHTMLTemplate(data)
	
	// Set content type and write response
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	rw.Write([]byte(html))
}

// getHTMLTemplate returns the HTML dashboard template with data inserted
func getHTMLTemplate(data MetricsData) string {
	// HTML header and style
	html := getHTMLHeader()
	
	// Start body content
	html += `
<body>
    <div class="header">
        <h1>K8Trust WebSocket Balancer Metrics</h1>
        <div class="controls">
            <span class="timestamp">` + data.Timestamp + `</span>
            <div class="auto-refresh">
                <input type="checkbox" id="auto-refresh" checked>
                <label for="auto-refresh">Auto-refresh</label>
                <select id="refresh-interval">
                    <option value="5"` + (func() string {
						if data.RefreshInterval == 5 {
							return " selected"
						}
						return ""
					})() + `>5s</option>
                    <option value="10"` + (func() string {
						if data.RefreshInterval == 10 {
							return " selected"
						}
						return ""
					})() + `>10s</option>
                    <option value="30"` + (func() string {
						if data.RefreshInterval == 30 {
							return " selected"
						}
						return ""
					})() + `>30s</option>
                    <option value="60"` + (func() string {
						if data.RefreshInterval == 60 {
							return " selected"
						}
						return ""
					})() + `>60s</option>
                </select>
                <span id="refresh-status" class="refresh-status"></span>
            </div>
            <button class="refresh-button" onclick="manualRefresh()">Refresh</button>
        </div>
    </div>
    
    <div class="summary">
        <h2>Summary</h2>
        <div class="summary-grid">
            <div class="metric-card">
                <div class="metric-label">Total Connections</div>
                <div class="metric-value">` + fmt.Sprintf("%d", data.TotalConnections) + `</div>
            </div>
            <div class="metric-card">
                <div class="metric-label">Services</div>
                <div class="metric-value">` + fmt.Sprintf("%d", data.ServiceCount) + `</div>
            </div>
            <div class="metric-card">
                <div class="metric-label">Pods</div>
                <div class="metric-value">` + fmt.Sprintf("%d", data.PodCount) + `</div>
            </div>
            <div class="metric-card">
                <div class="metric-label">Last Updated</div>
                <div class="metric-value" style="font-size: 1.2em">` + getJerusalemTime().Format("15:04:05") + `</div>
            </div>
        </div>
        <div class="refresh-info">
            Cache refreshes every <span>` + fmt.Sprintf("%d", data.RefreshInterval) + `s</span> with a forced full refresh every <span>` + fmt.Sprintf("%d", data.ForcedRefreshInterval) + `s</span>
        </div>
    </div>
    
    <div class="services">
        <h2>Services & Pods</h2>
        <table>
            <thead>
                <tr>
                    <th>Service</th>
                    <th>Connections</th>
                </tr>
            </thead>
            <tbody>`

	// Add service and pod data to the table
	for _, service := range data.Services {
		// Extract a shorter service name from URL for display
		serviceName := service.URL
		if parts := strings.Split(strings.TrimPrefix(service.URL, "http://"), "."); len(parts) > 0 {
			serviceName = parts[0]
		}
		
		html += `
                <tr>
                    <td><strong>` + serviceName + `</strong><br><span class="pod-details">` + service.URL + `</span></td>
                    <td class="connection-count">` + fmt.Sprintf("%d", service.Connections) + `</td>
                </tr>`
		
		// Add pod details if available
		if pods, ok := data.PodMetricsMap[service.URL]; ok && len(pods) > 0 {
			html += `
                <tr>
                    <td colspan="2">
                        <table class="pod-table">
                            <thead>
                                <tr>
                                    <th>Pod Name</th>
                                    <th>Connections</th>
                                    <th>Pod IP</th>
                                    <th>Node</th>
                                </tr>
                            </thead>
                            <tbody>`
			
			for _, pod := range pods {
				// Use placeholder if pod name is empty
				podName := pod.PodName
				if podName == "" {
					podName = "unknown"
				}
				
				html += `
                                <tr class="pod-row">
                                    <td>` + podName + `</td>
                                    <td class="connection-count">` + fmt.Sprintf("%d", pod.AgentsConnections) + `</td>
                                    <td>` + pod.PodIP + `</td>
                                    <td>` + pod.NodeName + `</td>
                                </tr>`
			}
			
			html += `
                            </tbody>
                        </table>
                    </td>
                </tr>`
		}
	}

	html += `
            </tbody>
        </table>
        
        <div class="json-link">
            <a href="` + data.BalancerMetricPath + `?format=json">View as JSON</a>
        </div>
    </div>`
    
	// Add JavaScript and close HTML
	html += getJavaScript(data.RefreshInterval)
	html += `
</body>
</html>`

	return html
}

// PrepareMetricsData takes raw metrics data and prepares it for rendering
func PrepareMetricsData(
	serviceConnections map[string]int,
	podMetricsMap map[string][]PodMetrics,
	balancerMetricPath string,
) MetricsData {
	// Calculate total connections
	totalConnections := 0
	for _, count := range serviceConnections {
		totalConnections += count
	}
	
	// Count total pods
	totalPods := 0
	for _, pods := range podMetricsMap {
		totalPods += len(pods)
	}
	
	// Sort services by name for consistent display
	type ServiceWithURL struct {
		URL  string
		Name string
	}
	sortedServices := make([]ServiceWithURL, 0, len(serviceConnections))
	for url := range serviceConnections {
		// Extract a shorter service name from URL for display
		serviceName := url
		if parts := strings.Split(strings.TrimPrefix(url, "http://"), "."); len(parts) > 0 {
			serviceName = parts[0]
		}
		sortedServices = append(sortedServices, ServiceWithURL{URL: url, Name: serviceName})
	}
	
	// Sort by service name
	sort.Slice(sortedServices, func(i, j int) bool {
		return sortedServices[i].Name < sortedServices[j].Name
	})
	
	// Convert to ServiceMetric objects
	services := make([]ServiceMetric, 0, len(sortedServices))
	for _, service := range sortedServices {
		services = append(services, ServiceMetric{
			URL:         service.URL,
			Connections: serviceConnections[service.URL],
		})
	}
	
	// Sort pods by name within each service
	for url, pods := range podMetricsMap {
		sort.Slice(pods, func(i, j int) bool {
			return pods[i].PodName < pods[j].PodName
		})
		podMetricsMap[url] = pods
	}
	
	return MetricsData{
		Timestamp:           getJerusalemTime().Format(time.RFC3339),
		Services:            services,
		PodMetricsMap:       podMetricsMap,
		TotalConnections:    totalConnections,
		ServiceCount:        len(services),
		PodCount:            totalPods,
		BalancerMetricPath:  balancerMetricPath,
		RefreshInterval:     10,
		ForcedRefreshInterval: 30,
	}
}

// getJerusalemTime returns the current time in Jerusalem (Israel) time zone
func getJerusalemTime() time.Time {
	loc, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil {
		// Fallback to manual UTC+3 if time zone data not available
		return time.Now().UTC().Add(3 * time.Hour)
	}
	return time.Now().In(loc)
}