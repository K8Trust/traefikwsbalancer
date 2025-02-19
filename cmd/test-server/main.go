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

    mux := http.NewServeMux()
    mux.HandleFunc("/ws", handleWebSocket)
    mux.HandleFunc("/metric", handleMetrics)
    mux.HandleFunc("/health", handleHealth)

    addr := fmt.Sprintf(":%d", port)
    log.Printf("Starting test server on %s", addr)
    log.Fatal(http.ListenAndServe(addr, mux))
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
    upgrader := ws.Upgrader{
        CheckOrigin: func(r *http.Request) bool { return true },
    }

    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        log.Printf("Upgrade failed: %v", err)
        return
    }
    defer conn.Close()

    activeConnections.Add(1)
    defer activeConnections.Add(-1)

    log.Printf("New WebSocket connection. Total active: %d", activeConnections.Load())

    for {
        messageType, message, err := conn.ReadMessage()
        if err != nil {
            log.Printf("Read error: %v", err)
            break
        }

        log.Printf("Received: %s", message)

        // Echo the message back
        err = conn.WriteMessage(messageType, message)
        if err != nil {
            log.Printf("Write error: %v", err)
            break
        }
    }
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
    metrics := struct {
        AgentsConnections int64 `json:"agentsConnections"`
    }{
        AgentsConnections: activeConnections.Load(),
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(metrics)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    w.Write([]byte("OK"))
}