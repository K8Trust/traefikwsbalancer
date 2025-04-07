package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/K8Trust/traefikwsbalancer/ws"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// WebSocket connection counter as a Prometheus gauge
	wsConnections prometheus.Gauge

	// Track connection count for our legacy API endpoint too
	connectionCount uint32
)

func main() {
	// Get service information from environment variables
	serviceName := os.Getenv("SERVICE_NAME")
	if serviceName == "" {
		serviceName = "websocket-service"
	}
	
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		// Try to get pod name from hostname as a fallback
		var err error
		podName, err = os.Hostname()
		if err != nil {
			podName = "unknown-pod"
		}
	}
	
	podIP := os.Getenv("POD_IP")
	nodeName := os.Getenv("NODE_NAME")
	
	// Configure WebSocket upgrader
	upgrader := ws.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		HandshakeTimeout: 2 * time.Second,
		ReadBufferSize:   1024,
		WriteBufferSize:  1024,
	}
	
	// Create a Prometheus gauge with service and pod labels
	wsConnections = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "websocket_connections_total",
			Help: "The current number of active WebSocket connections",
			ConstLabels: prometheus.Labels{
				"service": serviceName,
				"pod":     podName,
				"pod_ip":  podIP,
				"node":    nodeName,
			},
		},
	)
	
	// WebSocket handler
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		// Upgrade connection to WebSocket
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("Failed to upgrade connection: %v", err)
			return
		}
		
		// Increment connection counters
		atomic.AddUint32(&connectionCount, 1)
		wsConnections.Inc()
		
		log.Printf("WebSocket connection established, current count: %d", 
			atomic.LoadUint32(&connectionCount))
		
		// Ensure we decrement counters when the connection closes
		defer func() {
			conn.Close()
			atomic.AddUint32(&connectionCount, ^uint32(0)) // decrement
			wsConnections.Dec()
			log.Printf("WebSocket connection closed, current count: %d", 
				atomic.LoadUint32(&connectionCount))
		}()
		
		// Handle WebSocket communication
		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("Read error: %v", err)
				break
			}
			
			// Echo the message back
			if err := conn.WriteMessage(messageType, message); err != nil {
				log.Printf("Write error: %v", err)
				break
			}
		}
	})
	
	// Legacy metrics endpoint for backward compatibility
	http.HandleFunc("/metric", func(w http.ResponseWriter, r *http.Request) {
		count := atomic.LoadUint32(&connectionCount)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fmt.Sprintf(`{
			"agentsConnections": %d,
			"podName": "%s",
			"podIP": "%s",
			"nodeName": "%s"
		}`, count, podName, podIP, nodeName)))
	})
	
	// Prometheus metrics endpoint
	http.Handle("/metrics", promhttp.Handler())
	
	// Start the server
	log.Printf("Starting WebSocket server on :8080")
	log.Printf("Service: %s, Pod: %s, Node: %s", serviceName, podName, nodeName)
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}