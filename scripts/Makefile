.PHONY: build run test clean docker-build fmt lint check help

BINARY_NAME=bin/server
MAIN_FILE=cmd/server/main.go
MODULE_NAME=github.com/wanglongan587/cloud

help:
	@echo "Available commands:"
	@echo "  make fmt          - Format code using gofumpt & goimports"
	@echo "  make lint         - Run static analysis via golangci-lint"
	@echo "  make check        - Run full quality check (fmt + lint + test)"
	@echo "  make run          - Run service locally"
	@echo "  make build        - Compile the binary"
	@echo "  make test         - Run all unit tests"
	@echo "  make clean        - Remove build artifacts and logs"
	@echo "  make docker-build - Build Docker image"

fmt:
	go run mvdan.cc/gofumpt -w -extra .
	go run golang.org/x/tools/cmd/goimports -w -local $(MODULE_NAME) .

lint:
	go run github.com/golangci/golangci-lint/cmd/golangci-lint run -c .golangci.yml

check: fmt lint test

run:
	go run $(MAIN_FILE)

build:
	go build -ldflags="-s -w" -o $(BINARY_NAME) $(MAIN_FILE)

test:
	go test -v ./...

clean:
	rm -rf bin/ logs/ *.db coverage.out

docker-build:
	docker build -t cloud-service:latest -f scripts/Dockerfile .
