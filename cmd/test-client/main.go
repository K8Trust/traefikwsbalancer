package main

import (
    "flag"
    "log"
    "os"
    "os/signal"
    "time"

    "github.com/gorilla/websocket"
)

func main() {
    // Add command line flag for URL
    url := flag.String("url", "ws://localhost:8080/ws", "WebSocket server URL")
    flag.Parse()

    interrupt := make(chan os.Signal, 1)
    signal.Notify(interrupt, os.Interrupt)

    log.Printf("Connecting to %s", *url)
    
    headers := make(map[string][]string)
    headers["Agent-ID"] = []string{"test-client"}
    
    c, _, err := websocket.DefaultDialer.Dial(*url, headers)
    if err != nil {
        log.Fatal("dial:", err)
    }
    defer c.Close()

    done := make(chan struct{})

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

    ticker := time.NewTicker(time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-done:
            return
        case <-ticker.C:
            err := c.WriteMessage(websocket.TextMessage, []byte("Hello, Server!"))
            if err != nil {
                log.Println("write:", err)
                return
            }
        case <-interrupt:
            log.Println("interrupt")
            err := c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
            if err != nil {
                log.Println("write close:", err)
                return
            }
            select {
            case <-done:
            case <-time.After(time.Second):
            }
            return
        }
    }
}