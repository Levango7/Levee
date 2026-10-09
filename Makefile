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
# Go binary embeds the real assets. The dist/ contents are committed (there is
# no .gitignore under it); this target mirrors web/dist over them.
#
# The CRLF pre-check is not cosmetic. @vitejs/plugin-vue hashes the SOURCE BYTES
# into each component's scoped-style id (data-v-<hash>), so a CRLF working copy
# of a .vue/.ts file produces different chunk hashes than CI's LF checkout, and
# the CI frontend job rebuilds and requires the committed bundle to match byte
# for byte. Measured 2026-10-10: CRLF -> ChangesView-h12LhjpQ.js, LF ->
# ChangesView-Du5DbgNu.js for the same file. .gitattributes pins these types to
# LF at the boundary, but a file written by an editor or tool can still be CRLF
# in the working tree. The failure has no local symptom: a rebuild from the same
# CRLF tree matches itself, so `diff -r web/dist internal/web/dist` is clean
# while the pushed artifacts are stale. Refuse up front, with the one-shot fix.
web:
	@crlf=$$(find web/src -type f \( -name '*.ts' -o -name '*.vue' -o -name '*.css' \) \
		-exec grep -lU "$$(printf '')" {} + 2>/dev/null); \
	if [ -n "$$crlf" ]; then \
		echo "error: web/src has CRLF files, so the bundle would not match CI's LF build:"; \
		echo "$$crlf"; \
		echo "hint: git -c core.autocrlf=false checkout -- web/src   (or set core.autocrlf=false for this repo)"; \
		exit 1; \
	fi
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

# Baseline: golangci-lint v2.13 — keep in lockstep with the CI lint job
# (.github/workflows/ci.yml). Version drift between local and CI silently
# changes which issues the gate enforces.
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