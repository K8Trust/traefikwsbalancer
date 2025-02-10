package connectionbalancer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// MockFetcher is a mock implementation of ConnectionFetcher for testing.
type MockFetcher struct {
	MockURL string
}

// GetConnections returns the test server URL instead of a real pod.
func (m *MockFetcher) GetConnections(_ string) (int, error) {
	return 1, nil // ✅ Simulate that a pod is available.
}

func TestGetConnections(t *testing.T) {
	cb := &ConnectionBalancer{
		client:     &http.Client{},
		metricPath: "/metric",
		fetcher:    &MockFetcher{},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"agentsConnections": 5}`))
	}))
	defer server.Close()

	connections, err := cb.getConnections(server.URL)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if connections != 1 {
		t.Fatalf("Expected 1 connection, got %d", connections)
	}
}

func TestServeHTTP(t *testing.T) {
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Response from backend"))
	}))
	defer backendServer.Close()

	cb := &ConnectionBalancer{
		next: http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			rw.WriteHeader(http.StatusOK)
		}),
		client: &http.Client{},
		fetcher: &MockFetcher{
			MockURL: backendServer.URL,
		},
		pods: []string{backendServer.URL},
	}

	req := httptest.NewRequest("GET", "http://example.com", nil)
	w := httptest.NewRecorder()

	cb.ServeHTTP(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", w.Result().StatusCode)
	}

	body, _ := io.ReadAll(w.Result().Body)
	expectedBody := "Response from backend"
	if string(body) != expectedBody {
		t.Fatalf("Expected response body '%s', got '%s'", expectedBody, string(body))
	}
}

func TestWebSocketConnection(t *testing.T) {
	done := make(chan struct{})
	
	// Create a backend WebSocket server
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("Backend upgrade failed: %v", err)
			return
		}
		defer c.Close()

		// Echo the first message received and signal completion
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

	// Create the connection balancer
	cb := &ConnectionBalancer{
		client: &http.Client{},
		fetcher: &MockFetcher{
			MockURL: backendServer.URL,
		},
		pods: []string{backendServer.URL},
	}

	// Create a test server with the connection balancer
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cb.ServeHTTP(w, r)
	}))
	defer testServer.Close()

	// Convert http URL to ws URL
	wsURL := "ws" + strings.TrimPrefix(testServer.URL, "http")

	// Connect with minimal headers
	headers := http.Header{}
	headers.Set("Agent-ID", "test-agent")
	
	// Establish WebSocket connection
	c, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
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

	// Send test message
	testMessage := []byte("Hello, WebSocket!")
	if err := c.WriteMessage(websocket.TextMessage, testMessage); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Wait for response or timeout
	select {
	case <-done:
		// Success - message was echoed
	case <-time.After(time.Second):
		t.Fatal("Test timed out waiting for response")
	}

	// Read response
	_, message, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if string(message) != string(testMessage) {
		t.Fatalf("Expected message %q, got %q", testMessage, message)
	}
}