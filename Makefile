.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

PKG     := github.com/ashdaily/spoofy
LDFLAGS := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into ./bin
	@mkdir -p bin
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/spoofy ./cmd/spoofy
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/petstore ./examples/petstore
	@echo "built bin/spoofy $(VERSION)"

.PHONY: test
test: ## Run all tests with the race detector
	go test -race ./...

.PHONY: test-short
test-short: ## Skip timing-sensitive tests
	go test -short ./...

.PHONY: cover
cover: ## Write and summarise a coverage profile
	go test -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -1
	@echo "html report: go tool cover -html=coverage.out"

.PHONY: lint
lint: ## gofmt and go vet
	@unformatted=$$(gofmt -l ./cmd ./internal ./examples); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...
	@echo "lint ok"

.PHONY: tidy
tidy: ## Tidy and verify modules
	go mod tidy
	go mod verify

.PHONY: run
run: build ## Run against the bundled demo API
	@./bin/petstore -addr :8080 & echo $$! > /tmp/spoofy-petstore.pid
	@sleep 1
	@./bin/spoofy run \
		--spec http://localhost:8080/openapi.yaml \
		--url http://localhost:8080/v1 \
		--rate 20/s --shape diurnal \
		|| true
	@kill $$(cat /tmp/spoofy-petstore.pid) 2>/dev/null || true
	@rm -f /tmp/spoofy-petstore.pid

.PHONY: demo
demo: ## Bring up the full stack: API, Spoofy, Prometheus, Grafana
	docker compose up --build

.PHONY: demo-down
demo-down: ## Tear the demo stack down
	docker compose down -v

.PHONY: docker
docker: ## Build the container image
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t spoofy:$(VERSION) -t spoofy:latest .

.PHONY: clean
clean: ## Remove build output
	rm -rf bin coverage.out
