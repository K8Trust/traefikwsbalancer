//go:build !yaegi
// +build !yaegi

package main

import (
	"context"
	"log"
	"net/http"

	"github.com/K8Trust/traefikwsbalancer"
)

func main() {
	config := traefikwsbalancer.CreateConfig()
	config.Services = []string{
		"http://localhost:8081",
		"http://localhost:8082",
	}
	balancer, err := traefikwsbalancer.New(context.Background(), nil, config, "test-balancer")
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Starting balancer on :8080")
	log.Fatal(http.ListenAndServe(":8080", balancer))
}
