BINARY   := levee
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  := -ldflags "-s -w -X main.version=$(VERSION)"
GOFLAGS  := -trimpath
TARGETS  := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: all build web test lint clean cross-build run

all: lint test build

# `build` does NOT require node/npm: it compiles against the committed
# internal/web/dist assets. Run `make web` first when the frontend changed.
build:
	go build $(GOFLAGS) $(LDFLAGS) -o $(BINARY) ./cmd/levee

# Build the Web UI (npm ci + vite build) and refresh internal/web/dist so the
# Go binary embeds the real assets. Preserves internal/web/dist/.gitignore.
web:
	cd web && npm ci && npm run build
	find internal/web/dist -mindepth 1 ! -name '.gitignore' -exec rm -rf {} +
	cp -r web/dist/. internal/web/dist/

run:
	go run ./cmd/levee $(ARGS)

test:
	go test -race -cover ./...

test-integration:
	go test -race -tags=integration ./tests/integration/...

test-e2e:
	go test -race -tags=e2e ./tests/e2e/...

lint:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...

cross-build:
	@for target in $(TARGETS); do \
		os=$${target%/*}; \
		arch=$${target#*/}; \
		echo "Building $$os/$$arch..."; \
		GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) $(LDFLAGS) -o dist/$(BINARY)-$$os-$$arch ./cmd/levee; \
	done

clean:
	rm -f $(BINARY)
	rm -rf dist/
	go clean -testcache

tidy:
	go mod tidy

fmt:
	gofmt -s -w .
	goimports -w .

.PHONY: check
check: lint test
	@echo "All checks passed."