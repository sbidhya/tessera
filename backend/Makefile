# Tessera backend — developer tasks.
# Run from the backend/ directory: `make test`, `make run`, etc.

BINARY := bin/tessera
PKG    := ./...

.PHONY: all build run test race vet fmt tidy clean help

all: fmt vet test build ## Format, vet, test, and build

build: ## Compile the server to bin/tessera
	go build -o $(BINARY) ./cmd/tessera

run: ## Run the server (Ctrl-C to stop)
	go run ./cmd/tessera

test: ## Run all tests
	go test $(PKG)

race: ## Run all tests under the race detector
	go test -race $(PKG)

vet: ## Static analysis
	go vet $(PKG)

fmt: ## Format all Go source
	gofmt -w -l .

tidy: ## Tidy module dependencies
	go mod tidy

clean: ## Remove build artifacts
	rm -rf bin/

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
