// traefikwsbalancer_test.go
package traefikwsbalancer_test

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "strings"
    "sync"
    "sync/atomic"
    "testing"
    "time"

    "github.com/K8Trust/traefikwsbalancer"
    "github.com/K8Trust/traefikwsbalancer/ws"
)

type testServer struct {
    *httptest.Server
    connections atomic.Int64
    done       chan struct{}
    wg         sync.WaitGroup
}

func setupTestServer(t *testing.T) *testServer {
    ts := &testServer{
        done: make(chan struct{}),
    }
    
    mux := http.NewServeMux()
    
    // WebSocket handler
    mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
        ts.wg.Add(1)
        defer ts.wg.Done()

        upgrader := ws.Upgrader{
            CheckOrigin: func(r *http.Request) bool { return true },
        }

        c, err := upgrader.Upgrade(w, r, nil)
        if err != nil {
            t.Logf("Upgrade failed: %v", err)
            return
        }
        defer c.Close()

        ts.connections.Add(1)
        defer ts.connections.Add(-1)

        // Signal we're ready for messages
        ts.wg.Done()

        // Keep connection alive until test is done
        done := make(chan struct{})
        go func() {
            defer close(done)
            for {
                messageType, message, err := c.ReadMessage()
                if err != nil {
                    t.Logf("Read failed: %v", err)
                    return
                }

                if err := c.WriteMessage(messageType, message); err != nil {
                    t.Logf("Write failed: %v", err)
                    return
                }
            }
        }()

        select {
        case <-ts.done:
            return
        case <-done:
            return
        }
    })

    // Metrics handler
    mux.HandleFunc("/metric", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]int64{
            "agentsConnections": ts.connections.Load(),
        })
    })

    ts.Server = httptest.NewServer(mux)
    return ts
}

func TestWebSocketBalancer(t *testing.T) {
    // Setup test servers
    server1 := setupTestServer(t)
    defer server1.Close()
    defer close(server1.done)
    
    server2 := setupTestServer(t)
    defer server2.Close()
    defer close(server2.done)

    // Create balancer config
    config := traefikwsbalancer.CreateConfig()
    config.Services = []string{
        server1.URL,
        server2.URL,
    }

    // Create balancer
    balancer, err := traefikwsbalancer.New(context.Background(), nil, config, "test")
    if err != nil {
        t.Fatalf("Failed to create balancer: %v", err)
    }

    // Create test server with balancer
    ts := httptest.NewServer(balancer)
    defer ts.Close()

    // Create WebSocket URL
    wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

    // Connect WebSocket client
    headers := http.Header{}
    headers.Set("Test-Header", "test-value")

    dialer := ws.Dialer{
        HandshakeTimeout: 2 * time.Second,
    }

    // Add a small delay to allow metrics to initialize
    time.Sleep(100 * time.Millisecond)

    c, resp, err := dialer.Dial(wsURL+"/ws", headers)
    if err != nil {
        t.Fatalf("Dial failed: %v", err)
        if resp != nil {
            t.Logf("Response Status: %d", resp.StatusCode)
        }
        return
    }
    defer c.Close()

    // Wait for connection to be fully established
    time.Sleep(100 * time.Millisecond)

    // Send test message
    testMessage := []byte("Hello, WebSocket!")
    if err := c.WriteMessage(ws.TextMessage, testMessage); err != nil {
        t.Fatalf("Write failed: %v", err)
    }

    // Read response
    messageType, message, err := c.ReadMessage()
    if err != nil {
        t.Fatalf("Read failed: %v", err)
    }

    if messageType != ws.TextMessage {
        t.Errorf("Expected message type %d, got %d", ws.TextMessage, messageType)
    }

    if string(message) != string(testMessage) {
        t.Errorf("Expected message %q, got %q", testMessage, message)
    }

    // Verify connection distribution
    totalConnections := server1.connections.Load() + server2.connections.Load()
    if totalConnections != 1 {
        t.Errorf("Expected total connections to be 1, got %d", totalConnections)
        t.Logf("Server1 connections: %d", server1.connections.Load())
        t.Logf("Server2 connections: %d", server2.connections.Load())
    }
}

func TestHTTPBalancer(t *testing.T) {
    // Setup test servers
    server1 := setupTestServer(t)
    defer server1.Close()
    defer close(server1.done)
    
    server2 := setupTestServer(t)
    defer server2.Close()
    defer close(server2.done)

    // Create balancer config
    config := traefikwsbalancer.CreateConfig()
    config.Services = []string{
        server1.URL,
        server2.URL,
    }

    // Create balancer
    balancer, err := traefikwsbalancer.New(context.Background(), nil, config, "test")
    if err != nil {
        t.Fatalf("Failed to create balancer: %v", err)
    }

    // Create test request
    req := httptest.NewRequest("GET", "/metric", nil)
    w := httptest.NewRecorder()

    // Send request through balancer
    balancer.ServeHTTP(w, req)

    if w.Code != http.StatusOK {
        t.Errorf("Expected status code %d, got %d", http.StatusOK, w.Code)
    }

    var response struct {
        AgentsConnections int64 `json:"agentsConnections"`
    }
    if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
        t.Fatalf("Failed to decode response: %v", err)
    }
}