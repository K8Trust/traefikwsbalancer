package traefikwsbalancer_test

import (
	"io"
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

			connections, _, err := cb.GetConnections(server.URL)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetConnections() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && connections != tt.connections {
				t.Errorf("GetConnections() = %v, want %v", connections, tt.connections)
			}
		})
	}
}

// FixedBalancer is a simple HTTP handler that always routes to a fixed backend
type FixedBalancer struct {
	BackendURL string
	Client     *http.Client
}

func (f *FixedBalancer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if ws.IsWebSocketUpgrade(req) {
		f.handleWebSocket(rw, req)
		return
	}
	f.handleHTTP(rw, req)
}

func (f *FixedBalancer) handleWebSocket(rw http.ResponseWriter, req *http.Request) {
	dialer := ws.Dialer{
		HandshakeTimeout: 2 * time.Second,
		ReadBufferSize:   1024,
		WriteBufferSize:  1024,
	}

	targetURL := "ws" + strings.TrimPrefix(f.BackendURL, "http") + req.URL.Path
	if req.URL.RawQuery != "" {
		targetURL += "?" + req.URL.RawQuery
	}

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
		HandshakeTimeout: 2 * time.Second,
		ReadBufferSize:   1024,
		WriteBufferSize:  1024,
		CheckOrigin:      func(r *http.Request) bool { return true },
	}

	clientConn, err := upgrader.Upgrade(rw, req, nil)
	if err != nil {
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

func (f *FixedBalancer) handleHTTP(rw http.ResponseWriter, req *http.Request) {
	targetURL := f.BackendURL + req.URL.Path
	proxyReq, err := http.NewRequest(req.Method, targetURL, req.Body)
	if err != nil {
		http.Error(rw, "Failed to create proxy request", http.StatusInternalServerError)
		return
	}

	copyHeaders(proxyReq.Header, req.Header)
	proxyReq.Host = req.Host
	proxyReq.URL.RawQuery = req.URL.RawQuery

	resp, err := f.Client.Do(proxyReq)
	if err != nil {
		http.Error(rw, "Request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)
	io.Copy(rw, resp.Body)
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

	// Create a simplified balancer that just forwards to our test backend
	fb := &FixedBalancer{
		BackendURL: backendServer.URL,
		Client:     &http.Client{Timeout: 2 * time.Second},
	}

	testServer := httptest.NewServer(fb)
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

// Helper function to copy headers
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}