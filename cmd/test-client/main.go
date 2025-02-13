package main

import (
	"flag"
	"log"
	"time"

	"github.com/K8Trust/traefikwsbalancer/ws"
)

func main() {
	url := flag.String("url", "ws://localhost:8080/ws", "WebSocket server URL")
	flag.Parse()

	log.Printf("Connecting to %s", *url)

	headers := make(map[string][]string)
	headers["Agent-ID"] = []string{"test-client"}

	c, _, err := ws.DefaultDialer.Dial(*url, headers)
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
			err := c.WriteMessage(ws.TextMessage, []byte("Hello, Server!"))
			if err != nil {
				log.Println("write:", err)
				return
			}
		}
	}
}
