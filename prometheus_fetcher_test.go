package traefikwsbalancer_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/K8Trust/traefikwsbalancer"
)

func TestPrometheusConnectionFetcher(t *testing.T) {
	// Mock Prometheus server
	mockPromServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("Expected path /api/v1/query, got %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}

		query := r.URL.Query().Get("query")
		if query == "" {
			t.Error("Missing query parameter")
			http.Error(w, "Missing query", http.StatusBadRequest)
			return
		}

		// Check what service is being queried
		serviceName := "websocket-service"
		if query == "websocket_connections_total{service=\"service-1\"}" {
			serviceName = "service-1"
		} else if query == "websocket_connections_total{service=\"service-2\"}" {
			serviceName = "service-2"
		}

		// Respond with mock data based on service name
		var result map[string]interface{}
		if serviceName == "service-1" {
			result = map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"resultType": "vector",
					"result": []map[string]interface{}{
						{
							"metric": map[string]string{
								"service": "service-1",
								"pod":     "pod-service-1-0",
								"pod_ip":  "10.0.0.1",
								"node":    "node-1",
							},
							"value": []interface{}{
								1234567890, // timestamp
								"5",        // connection count as string
							},
						},
						{
							"metric": map[string]string{
								"service": "service-1",
								"pod":     "pod-service-1-1",
								"pod_ip":  "10.0.0.2",
								"node":    "node-2",
							},
							"value": []interface{}{
								1234567890, // timestamp
								"7",        // connection count as string
							},
						},
					},
				},
			}
		} else if serviceName == "service-2" {
			result = map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"resultType": "vector",
					"result": []map[string]interface{}{
						{
							"metric": map[string]string{
								"service": "service-2",
								"pod":     "pod-service-2-0",
								"pod_ip":  "10.0.0.3",
								"node":    "node-1",
							},
							"value": []interface{}{
								1234567890, // timestamp
								"3",        // connection count as string
							},
						},
					},
				},
			}
		} else {
			// Empty result (no pods)
			result = map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"resultType": "vector",
					"result":     []map[string]interface{}{},
				},
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}))
	defer mockPromServer.Close()

	// Create the connection fetcher
	fetcher := traefikwsbalancer.NewPrometheusConnectionFetcher(
		mockPromServer.URL,
		"websocket_connections_total",
		"service",
		"pod",
		5*time.Second,
	)

	t.Run("TestGetConnectionsService1", func(t *testing.T) {
		// Test service-1 with two pods
		connections, err := fetcher.GetConnections("http://service-1.default.svc.cluster.local:80")
		if err != nil {
			t.Fatalf("Expected no error, got %v", err)
		}
		
		// Should return the sum of connections across all pods (5 + 7 = 12)
		if connections != 12 {
			t.Errorf("Expected 12 connections, got %d", connections)
		}
	})

	t.Run("TestGetConnectionsService2", func(t *testing.T) {
		// Test service-2 with one pod
		connections, err := fetcher.GetConnections("http://service-2.default.svc.cluster.local:80")
		if err != nil {
			t.Fatalf("Expected no error, got %v", err)
		}
		
		// Should return connections from the single pod (3)
		if connections != 3 {
			t.Errorf("Expected 3 connections, got %d", connections)
		}
	})

	t.Run("TestGetAllPodsForService", func(t *testing.T) {
		// Test getting all pods for service-1
		pods, err := fetcher.GetAllPodsForService("http://service-1.default.svc.cluster.local:80")
		if err != nil {
			t.Fatalf("Expected no error, got %v", err)
		}
		
		// Should return data for both pods
		if len(pods) != 2 {
			t.Fatalf("Expected 2 pods, got %d", len(pods))
		}
		
		// Verify pod metrics
		if pods[0].PodName != "pod-service-1-0" || pods[0].AgentsConnections != 5 {
			t.Errorf("Invalid pod data: %+v", pods[0])
		}
		
		if pods[1].PodName != "pod-service-1-1" || pods[1].AgentsConnections != 7 {
			t.Errorf("Invalid pod data: %+v", pods[1])
		}
	})

	t.Run("TestServiceNotFound", func(t *testing.T) {
		// Test service with no pods
		connections, err := fetcher.GetConnections("http://unknown-service.default.svc.cluster.local:80")
		if err != nil {
			t.Fatalf("Expected no error even for empty results, got %v", err)
		}
		
		// Should return 0 connections when no pods are found
		if connections != 0 {
			t.Errorf("Expected 0 connections for unknown service, got %d", connections)
		}
	})
}