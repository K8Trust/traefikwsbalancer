package traefikwsbalancer_test

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/K8Trust/traefikwsbalancer"
	"github.com/K8Trust/traefikwsbalancer/ws"
)

// MockFetcher is a mock implementation of ConnectionFetcher for testing.
type MockFetcher struct {
	MockURL     string
	Connections int
	Error       error
}

// GetConnections returns a mock connection count or error.
func (m *MockFetcher) GetConnections(_ string) (int, error) {
	if m.Error != nil {
		return 0, m.Error
	}
	return m.Connections, nil
}

// MockK8sAPI is a mock implementation of the Kubernetes API for testing.
type MockK8sAPI struct {
	server *httptest.Server
}

func NewMockK8sAPI() *MockK8sAPI {
	m := &MockK8sAPI{}
	m.server = httptest.NewServer(http.HandlerFunc(m.handleRequest))
	return m
}

func (m *MockK8sAPI) Close() {
	m.server.Close()
}

func (m *MockK8sAPI) URL() string {
	return m.server.URL
}

func (m *MockK8sAPI) handleRequest(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/") {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	// Extract namespace and service name from path
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 6 {
		http.Error(w, "Invalid path format", http.StatusBadRequest)
		return
	}

	// We're using the namespace in the log message to avoid the unused variable warning
	namespace := parts[4]
	serviceName := parts[6]
	log.Printf("Handling request for namespace: %s, service: %s", namespace, serviceName)

	// Mock endpoints response
	endpoints := struct {
		Subsets []struct {
			Addresses []struct {
				IP   string `json:"ip"`
				TargetRef struct {
					Name string `json:"name"`
				} `json:"targetRef"`
			} `json:"addresses"`
		} `json:"subsets"`
	}{
		Subsets: []struct {
			Addresses []struct {
				IP   string `json:"ip"`
				TargetRef struct {
					Name string `json:"name"`
				} `json:"targetRef"`
			} `json:"addresses"`
		}{
			{
				Addresses: []struct {
					IP   string `json:"ip"`
					TargetRef struct {
						Name string `json:"name"`
					} `json:"targetRef"`
				}{
					{
						IP: "10.0.0.1",
						TargetRef: struct {
							Name string `json:"name"`
						}{
							Name: serviceName + "-pod-1",
						},
					},
					{
						IP: "10.0.0.2",
						TargetRef: struct {
							Name string `json:"name"`
						}{
							Name: serviceName + "-pod-2",
						},
					},
				},
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(endpoints)
}

func TestGetConnections(t *testing.T) {
	tests := []struct {
		name        string
		connections int
		wantErr     bool
	}{
		{
			name:        "successful fetch",
			connections: 5,
			wantErr:     false,
		},
		{
			name:        "zero connections",
			connections: 0,
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cb := &traefikwsbalancer.Balancer{
				Client:     &http.Client{Timeout: 5 * time.Second},
				MetricPath: "/metric",
				Fetcher: &MockFetcher{
					Connections: tt.connections,
				},
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"agentsConnections": 5}`))
			}))
			defer server.Close()

			connections, podMetrics, err := cb.GetConnections(server.URL)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetConnections() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && connections != tt.connections {
				t.Errorf("GetConnections() = %v, want %v", connections, tt.connections)
			}
			// We don't need to check podMetrics in this test as it's using the mock fetcher
			_ = podMetrics
		})
	}
}

func TestGetAllPodsForService(t *testing.T) {
	// Create mock Kubernetes API server
	mockK8s := NewMockK8sAPI()
	defer mockK8s.Close()

	// Create mock pod metrics server
	podServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metric" {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"agentsConnections": 5,
			"podName": "test-pod",
			"podIP": "10.0.0.1",
			"nodeName": "test-node"
		}`))
	}))
	defer podServer.Close()

	// Create balancer instance with mock Kubernetes API server
	cb := &traefikwsbalancer.Balancer{
		Client:             &http.Client{Timeout: 5 * time.Second},
		MetricPath:         "/metric",
		DiscoveryTimeout:   2 * time.Second,
		Services:           []string{"http://test-service.default.svc.cluster.local"},
		KubernetesAPIURL:  mockK8s.URL(), // Set the mock Kubernetes API URL
	}

	// Test pod discovery
	pods, err := cb.GetAllPodsForService("http://test-service.default.svc.cluster.local")
	if err != nil {
		t.Fatalf("GetAllPodsForService() error = %v", err)
	}

	if len(pods) == 0 {
		t.Fatal("GetAllPodsForService() returned no pods")
	}

	// Verify pod metrics
	for _, pod := range pods {
		if pod.PodName == "" {
			t.Error("Pod name is empty")
		}
		if pod.PodIP == "" {
			t.Error("Pod IP is empty")
		}
		if pod.AgentsConnections == 0 {
			t.Error("Pod has no connections")
		}
	}
}

func TestWebSocketConnection(t *testing.T) {
	done := make(chan struct{})

	upgrader := ws.Upgrader{
		CheckOrigin:      func(r *http.Request) bool { return true },
		HandshakeTimeout: 2 * time.Second,
		ReadBufferSize:   1024,
		WriteBufferSize:  1024,
	}

	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ws.IsWebSocketUpgrade(r) {
			t.Error("Expected WebSocket upgrade request")
			return
		}

		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("Backend upgrade failed: %v", err)
			return
		}
		defer c.Close()

		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		c.SetWriteDeadline(time.Now().Add(2 * time.Second))

		messageType, message, err := c.ReadMessage()
		if err != nil {
			t.Logf("Backend read failed: %v", err)
			return
		}

		err = c.WriteMessage(messageType, message)
		if err != nil {
			t.Logf("Backend write failed: %v", err)
			return
		}
		close(done)
	}))
	defer backendServer.Close()

	cb := &traefikwsbalancer.Balancer{
		Client: &http.Client{Timeout: 2 * time.Second},
		Fetcher: &MockFetcher{
			MockURL:     backendServer.URL,
			Connections: 1,
		},
		Services:     []string{backendServer.URL},
		DialTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		ReadTimeout:  2 * time.Second,
	}

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cb.ServeHTTP(w, r)
	}))
	defer testServer.Close()

	wsURL := "ws" + strings.TrimPrefix(testServer.URL, "http")

	headers := http.Header{}
	headers.Set("Agent-ID", "test-agent")

	dialer := ws.Dialer{
		HandshakeTimeout: 2 * time.Second,
		ReadBufferSize:   1024,
		WriteBufferSize:  1024,
	}

	c, resp, err := dialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
		if resp != nil {
			t.Logf("HTTP Response: %d", resp.StatusCode)
			body, _ := io.ReadAll(resp.Body)
			t.Logf("Response body: %s", body)
		}
		return
	}
	defer c.Close()

	testMessage := []byte("Hello, WebSocket!")
	if err := c.WriteMessage(ws.TextMessage, testMessage); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Test timed out waiting for response")
	}

	_, message, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if string(message) != string(testMessage) {
		t.Fatalf("Expected message %q, got %q", testMessage, message)
	}
}
