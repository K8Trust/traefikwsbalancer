// cmd/test-server/main.go
package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/K8Trust/traefikwsbalancer"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	HandshakeTimeout: 10 * time.Second,
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("Backend received WS request: %s %s", r.Method, r.URL.Path)
	log.Printf("Headers: %v", r.Header)

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Backend upgrade error: %v", err)
		return
	}
	defer c.Close()

	log.Printf("Backend WebSocket connection established")

	for {
		mt, message, err := c.ReadMessage()
		if err != nil {
			log.Printf("Backend read error: %v", err)
			break
		}
		log.Printf("Backend received message: %s", message)

		err = c.WriteMessage(mt, message)
		if err != nil {
			log.Printf("Backend write error: %v", err)
			break
		}
	}
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("Metrics request received: %s %s", r.Method, r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"agentsConnections": 0}`))
}

func main() {
	// Start backend servers
	for _, port := range []string{":8081", ":8082"} {
		go func(port string) {
			mux := http.NewServeMux()
			mux.HandleFunc("/ws", wsHandler)
			mux.HandleFunc("/metric", metricsHandler)
			log.Printf("Starting backend server on %s", port)
			if err := http.ListenAndServe(port, mux); err != nil {
				log.Printf("Backend server error on %s: %v", port, err)
			}
		}(port)
	}

	// Give backends time to start
	time.Sleep(time.Second)

	// Start balancer
	config := traefikwsbalancer.CreateConfig()
	config.Pods = []string{
		"http://localhost:8081",
		"http://localhost:8082",
	}
	
	balancer, err := traefikwsbalancer.New(context.Background(), nil, config, "")
	if err != nil {
		log.Fatal(err)
	}
	
	http.Handle("/", balancer)
	log.Printf("Starting balancer on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}