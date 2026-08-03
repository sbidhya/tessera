.PHONY: build test race vet fmt lint run health clean

BINARY := bin/tessera
PKG    := ./...

build:
	@mkdir -p bin
	go build -o $(BINARY) ./cmd/tessera
	@echo "built $(BINARY)"

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

# Run the server locally (uses env overrides via FromEnv)
run:
	go run ./cmd/tessera

# Curl the health endpoint (server must be running)
health:
	curl -s -i http://localhost:8080/healthz

clean:
	rm -rf bin

# Verify B0 gate: vet + race + health shape (health needs running server)
gate: vet race
	@echo "gate: vet + race passed (start server and run 'make health' for /healthz check)"
