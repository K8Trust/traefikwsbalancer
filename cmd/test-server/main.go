// cmd/test-server/main.go
package main

import (
    "encoding/json"
    "flag"
    "fmt"
    "log"
    "net/http"
    "sync/atomic"

    "github.com/K8Trust/traefikwsbalancer/ws"
)

var (
    activeConnections atomic.Int64
    port             int
)

func main() {
    flag.IntVar(&port, "port", 8081, "Port to listen on")
    flag.Parse()

    // Add periodic counter verification
    go func() {
        ticker := time.NewTicker(30 * time.Second)
        for range ticker.C {
            current := activeConnections.Load()
            log.Printf("[DEBUG] Periodic counter check. Current connections: %d", current)
            // Reset if suspiciously high
            if current > 100 {
                log.Printf("[WARN] Counter seems high, resetting")
                activeConnections.Store(0)
            }
        }
    }()

    mux := http.NewServeMux()
    mux.HandleFunc("/ws", handleWebSocket)
    mux.HandleFunc("/metric", handleMetrics)
    mux.HandleFunc("/health", handleHealth)

    // Add counter reset endpoint
    mux.HandleFunc("/reset", func(w http.ResponseWriter, r *http.Request) {
        if r.Method == "POST" {
            old := activeConnections.Swap(0)
            log.Printf("[INFO] Counter reset. Old value: %d", old)
            w.WriteHeader(http.StatusOK)
            return
        }
        w.WriteHeader(http.StatusMethodNotAllowed)
    })

    addr := fmt.Sprintf(":%d", port)
    log.Printf("Starting test server on %s", addr)
    log.Fatal(http.ListenAndServe(addr, mux))
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
    log.Printf("[DEBUG] Received WebSocket connection request")
    
    upgrader := ws.Upgrader{
        CheckOrigin: func(r *http.Request) bool { 
            log.Printf("[DEBUG] Checking origin: %v", r.Header.Get("Origin"))
            return true 
        },
    }

    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        log.Printf("[ERROR] Upgrade failed: %v", err)
        return
    }
    
    currentConns := activeConnections.Add(1)
    log.Printf("[INFO] New WebSocket connection. Current active: %d", currentConns)
    
    defer func() {
        conn.Close()
        currentConns := activeConnections.Add(-1)
        log.Printf("[INFO] Connection closed. Current active: %d", currentConns)
    }()

    for {
        messageType, message, err := conn.ReadMessage()
        if err != nil {
            log.Printf("[DEBUG] Read error (normal on client disconnect): %v", err)
            break
        }

        log.Printf("[DEBUG] Received message: %s", message)

        err = conn.WriteMessage(messageType, message)
        if err != nil {
            log.Printf("[ERROR] Write error: %v", err)
            break
        }
    }
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
    current := activeConnections.Load()
    log.Printf("[DEBUG] Metrics request. Current connections: %d", current)
    
    metrics := struct {
        AgentsConnections int64 `json:"agentsConnections"`
    }{
        AgentsConnections: current,
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(metrics)
}