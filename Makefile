# Makefile
.PHONY: lint test vendor tidy yaml-test clean all build

PLUGIN_NAME := traefikwsbalancer
VERSION := v0.1.0

all: clean lint test build

lint:
	golangci-lint run

test:
	go test -v -cover ./...

vendor:
	go mod vendor

tidy:
	go mod tidy

yaml-test:
	yamllint .traefik.yml

clean:
	rm -rf ./vendor
	rm -f coverage.out

build: tidy vendor
	go build -o $(PLUGIN_NAME)

# Traefik plugin specific targets
dev-plugin:
	traefik --config-file=dev.yml

validate-plugin:
	yaegi test -v github.com/K8Trust/traefikwsbalancer