package traefikwsbalancer_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/K8Trust/traefikwsbalancer"
)

// MockFetcher is a mock implementation of ConnectionFetcher for testing.
type MockFetcher struct {
	MockURL     string
	Connections int
	Error       error
}

// GetConnections returns a mock connection count or error
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

			connections, err := cb.GetConnections(server.URL)
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

func TestWebSocketConnection(t *testing.T) {
    done := make(chan struct{})

    upgrader := websocket.Upgrader{
        CheckOrigin: func(r *http.Request) bool { return true },
        HandshakeTimeout: 2 * time.Second,
    }

    backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !websocket.IsWebSocketUpgrade(r) {
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

    // Only set required headers
    headers := http.Header{}
    headers.Set("Agent-ID", "test-agent")

    dialer := websocket.Dialer{
        HandshakeTimeout: 2 * time.Second,
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
    if err := c.WriteMessage(websocket.TextMessage, testMessage); err != nil {
        t.Fatalf("Write failed: %v", err)
    }

    select {
    case <-done:
        // Success
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