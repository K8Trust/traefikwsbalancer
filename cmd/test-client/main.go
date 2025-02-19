// cmd/test-client/main.go
package main

import (
    "flag"
    "log"
    "time"

    "github.com/K8Trust/traefikwsbalancer/ws"
)

func main() {
    url := flag.String("url", "ws://localhost:8080/ws", "WebSocket server URL")
    id := flag.String("id", "test-client", "Client ID")
    flag.Parse()

    log.Printf("Connecting to %s with ID %s", *url, *id)

    headers := make(map[string][]string)
    headers["Client-ID"] = []string{*id}

    dialer := ws.Dialer{
        HandshakeTimeout: 10 * time.Second,
    }

    c, _, err := dialer.Dial(*url, headers)
    if err != nil {
        log.Fatal("dial:", err)
    }
    defer c.Close()

    done := make(chan struct{})

    // Read messages
    go func() {
        defer close(done)
        for {
            _, message, err := c.ReadMessage()
            if err != nil {
                log.Println("read:", err)
                return
            }
            log.Printf("Received: %s", message)
        }
    }()

    // Send messages periodically
    ticker := time.NewTicker(time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-done:
            return
        case t := <-ticker.C:
            message := []byte(t.String())
            err := c.WriteMessage(ws.TextMessage, message)
            if err != nil {
                log.Println("write:", err)
                return
            }
            log.Printf("Sent: %s", message)
        }
    }
}