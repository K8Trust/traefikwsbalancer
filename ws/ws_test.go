// ws/ws_test.go
package ws

import (
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"
)

func TestWebSocketUpgrade(t *testing.T) {
    upgrader := Upgrader{
        CheckOrigin: func(r *http.Request) bool { return true },
    }

    // Create test server
    s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !IsWebSocketUpgrade(r) {
            t.Error("Expected WebSocket upgrade request")
            return
        }

        c, err := upgrader.Upgrade(w, r, nil)
        if err != nil {
            t.Errorf("Upgrade failed: %v", err)
            return
        }
        defer c.Close()

        messageType, p, err := c.ReadMessage()
        if err != nil {
            t.Errorf("ReadMessage failed: %v", err)
            return
        }

        if err := c.WriteMessage(messageType, p); err != nil {
            t.Errorf("WriteMessage failed: %v", err)
            return
        }
    }))
    defer s.Close()

    // Create WebSocket URL
    wsURL := "ws" + strings.TrimPrefix(s.URL, "http")

    // Connect client
    dialer := Dialer{
        HandshakeTimeout: time.Second,
    }

    c, _, err := dialer.Dial(wsURL, nil)
    if err != nil {
        t.Fatalf("Dial failed: %v", err)
    }
    defer c.Close()

    // Send test message
    message := []byte("Hello, WebSocket!")
    if err := c.WriteMessage(TextMessage, message); err != nil {
        t.Fatalf("WriteMessage failed: %v", err)
    }

    // Read response
    messageType, p, err := c.ReadMessage()
    if err != nil {
        t.Fatalf("ReadMessage failed: %v", err)
    }

    if messageType != TextMessage {
        t.Errorf("Expected message type %d, got %d", TextMessage, messageType)
    }

    if string(p) != string(message) {
        t.Errorf("Expected message %q, got %q", message, p)
    }
}

func TestIsWebSocketUpgrade(t *testing.T) {
    tests := []struct {
        name     string
        headers  map[string]string
        expected bool
    }{
        {
            name: "valid upgrade",
            headers: map[string]string{
                "Upgrade":    "websocket",
                "Connection": "upgrade",
            },
            expected: true,
        },
        {
            name: "invalid upgrade",
            headers: map[string]string{
                "Upgrade":    "something",
                "Connection": "keep-alive",
            },
            expected: false,
        },
        {
            name:     "no headers",
            headers:  map[string]string{},
            expected: false,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            r := httptest.NewRequest("GET", "/", nil)
            for k, v := range tt.headers {
                r.Header.Set(k, v)
            }

            if got := IsWebSocketUpgrade(r); got != tt.expected {
                t.Errorf("IsWebSocketUpgrade() = %v, want %v", got, tt.expected)
            }
        })
    }
}

func TestWebSocketFraming(t *testing.T) {
    upgrader := Upgrader{
        CheckOrigin: func(r *http.Request) bool { return true },
    }

    s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        c, err := upgrader.Upgrade(w, r, nil)
        if err != nil {
            t.Errorf("Upgrade failed: %v", err)
            return
        }
        defer c.Close()

        // Test different message types
        messages := []struct {
            messageType int
            data       []byte
        }{
            {TextMessage, []byte("Hello")},
            {BinaryMessage, []byte{1, 2, 3, 4}},
            {TextMessage, []byte(strings.Repeat("A", 65536))}, // Large message
        }

        for _, msg := range messages {
            if err := c.WriteMessage(msg.messageType, msg.data); err != nil {
                t.Errorf("WriteMessage failed: %v", err)
                return
            }
        }
    }))
    defer s.Close()

    wsURL := "ws" + strings.TrimPrefix(s.URL, "http")
    c, _, err := Dialer{HandshakeTimeout: time.Second}.Dial(wsURL, nil)
    if err != nil {
        t.Fatalf("Dial failed: %v", err)
    }
    defer c.Close()

    messages := []struct {
        expectedType int
        expectedLen  int
    }{
        {TextMessage, 5},
        {BinaryMessage, 4},
        {TextMessage, 65536},
    }

    for i, expected := range messages {
        messageType, data, err := c.ReadMessage()
        if err != nil {
            t.Fatalf("ReadMessage %d failed: %v", i, err)
        }

        if messageType != expected.expectedType {
            t.Errorf("Message %d: expected type %d, got %d", i, expected.expectedType, messageType)
        }

        if len(data) != expected.expectedLen {
            t.Errorf("Message %d: expected length %d, got %d", i, expected.expectedLen, len(data))
        }
    }
}