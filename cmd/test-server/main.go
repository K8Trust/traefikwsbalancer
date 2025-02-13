package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/K8Trust/traefikwsbalancer"
	"github.com/K8Trust/traefikwsbalancer/ws"
)

var (
	upgrader = ws.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		HandshakeTimeout: 2 * time.Second,
		ReadBufferSize:   1024,
		WriteBufferSize:  1024,
	}

	connections1 uint32
	connections2 uint32
)

func wsHandler(counter *uint32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddUint32(counter, 1)
		defer atomic.AddUint32(counter, ^uint32(0)) // decrement

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
}

func metricsHandler(counter *uint32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		count := atomic.LoadUint32(counter)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fmt.Sprintf(`{"agentsConnections": %d}`, count)))
	}
}

func main() {
	// Start backend server 1.
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/ws", wsHandler(&connections1))
		mux.HandleFunc("/metric", metricsHandler(&connections1))
		log.Printf("Starting backend server 1 on :8081")
		if err := http.ListenAndServe(":8081", mux); err != nil {
			log.Printf("Backend server 1 error: %v", err)
		}
	}()

	// Start backend server 2.
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/ws", wsHandler(&connections2))
		mux.HandleFunc("/metric", metricsHandler(&connections2))
		log.Printf("Starting backend server 2 on :8082")
		if err := http.ListenAndServe(":8082", mux); err != nil {
			log.Printf("Backend server 2 error: %v", err)
		}
	}()

	// Allow backends to start.
	time.Sleep(time.Second)

	config := traefikwsbalancer.CreateConfig()
	config.Services = []string{
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