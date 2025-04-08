package traefikwsbalancer_test

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/K8Trust/traefikwsbalancer"
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
			// We expect podMetrics to be nil when using the mock fetcher
			if !tt.wantErr && podMetrics != nil {
				t.Errorf("Expected nil podMetrics when using mock fetcher, got %v", podMetrics)
			}
		})
	}
}

func TestWebSocketConnection(t *testing.T) {
	// Channel to signal when server has received and processed the request
	done := make(chan struct{})
	
	// Test message content
	testMessage := "Hello, WebSocket!"
	
	// Create a backend server that handles WebSocket connections
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ensure it's a WebSocket upgrade request
		if !isWebSocketRequest(r) {
			t.Error("Expected WebSocket upgrade request")
			return
		}
		
		// Upgrade the connection
		conn, err := upgradeWebSocketConnection(w, r)
		if err != nil {
			t.Errorf("Failed to upgrade WebSocket connection: %v", err)
			return
		}
		defer conn.Close()
		
		// Read a message
		buf := make([]byte, 1024)
		n, err := conn.Read(buf)
		if err != nil {
			t.Errorf("Error reading from WebSocket: %v", err)
			return
		}
		
		message := string(buf[:n])
		if !strings.Contains(message, testMessage) {
			t.Errorf("Unexpected message content: %s", message)
			return
		}
		
		// Echo the message back
		_, err = conn.Write(buf[:n])
		if err != nil {
			t.Errorf("Error writing to WebSocket: %v", err)
			return
		}
		
		// Signal that the echo is complete
		close(done)
	}))
	defer backendServer.Close()

	// Create a balancer for testing
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
		MaxRetries:   3,
		RetryDelay:   1,
	}

	// Manually initialize the connection cache for testing
	cb.InitializeConnectionCacheForTest(backendServer.URL, 1)

	// Create a test server with the balancer
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cb.ServeHTTP(w, r)
	}))
	defer testServer.Close()

	// Connect to the WebSocket endpoint
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	wsURL := "ws" + strings.TrimPrefix(testServer.URL, "http")
	conn, err := dialer.Dial("tcp", wsURL[5:]) // Remove "ws://" prefix
	if err != nil {
		t.Fatalf("Failed to connect to WebSocket server: %v", err)
		return
	}
	defer conn.Close()
	
	// Send WebSocket handshake
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	handshake := fmt.Sprintf(
		"GET / HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n"+
		"Agent-ID: test-agent\r\n"+
		"\r\n",
		wsURL[5:], key)
	
	_, err = conn.Write([]byte(handshake))
	if err != nil {
		t.Fatalf("Failed to send WebSocket handshake: %v", err)
		return
	}
	
	// Read handshake response
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read handshake response: %v", err)
		return
	}
	
	response := string(buf[:n])
	if !strings.Contains(response, "HTTP/1.1 101") {
		t.Fatalf("Not a WebSocket upgrade response: %s", response)
		return
	}
	
	// Send a test message
	msg := fmt.Sprintf("\x81%c%s", byte(len(testMessage)), testMessage)
	_, err = conn.Write([]byte(msg))
	if err != nil {
		t.Fatalf("Failed to send WebSocket message: %v", err)
		return
	}
	
	// Wait for server to process the message
	select {
	case <-done:
		// Successfully received echo
	case <-time.After(5 * time.Second):
		t.Fatal("Test timed out waiting for WebSocket echo")
	}
	
	// Read echo response
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read echo response: %v", err)
		return
	}
	
	// Verify the echoed message
	if n < 2 || !strings.Contains(string(buf[2:n]), testMessage) {
		t.Fatalf("Unexpected echo response: %s", string(buf[:n]))
	}
}

// Helper functions for WebSocket testing

func isWebSocketRequest(r *http.Request) bool {
	upgrade := r.Header.Get("Upgrade")
	connection := r.Header.Get("Connection")
	return strings.ToLower(upgrade) == "websocket" && strings.Contains(strings.ToLower(connection), "upgrade")
}

func upgradeWebSocketConnection(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("webserver doesn't support hijacking")
	}
	
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	
	// Send 101 Switching Protocols response
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
				"Upgrade: websocket\r\n" +
				"Connection: Upgrade\r\n" +
				"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n" +
				"\r\n"
	
	bufrw.WriteString(response)
	bufrw.Flush()
	
	return conn, nil
}
