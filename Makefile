.PHONY: build test race vet fmt run health clean gate

# Root Makefile delegates to backend/ so `make test` works from repo root.

build:
	$(MAKE) -C backend build

test:
	$(MAKE) -C backend test

race:
	$(MAKE) -C backend race

vet:
	$(MAKE) -C backend vet

fmt:
	$(MAKE) -C backend fmt

run:
	$(MAKE) -C backend run

health:
	$(MAKE) -C backend health

clean:
	$(MAKE) -C backend clean

gate:
	$(MAKE) -C backend gate

help:
	@echo "Targets:"
	@echo "  make build   - build backend/cmd/tessera"
	@echo "  make test    - go test ./..."
	@echo "  make race    - go test -race ./..."
	@echo "  make vet     - go vet ./..."
	@echo "  make run     - run server locally on :8080"
	@echo "  make health  - curl /healthz (server must be running)"
	@echo "  make gate    - vet + race (B0 gate checks)"
