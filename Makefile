.PHONY: all test lint build

all: tidy vendor build

tidy:
	go mod tidy

vendor:
	go mod vendor

lint:
	golangci-lint run --timeout=5m --modules-download-mode=vendor

test:
	go test -v ./...

build:
	go build -o connectionbalancer ./...
