.PHONY: build test lint fmt-check check integration run clean tidy

BIN := opod
PKG := ./cmd/opod
GOFLAGS ?= -trimpath

build:
	go build $(GOFLAGS) -o $(BIN) $(PKG)

test:
	go test ./...

lint:
	go vet ./...

# fmt-check fails if any .go file is not gofmt-s-clean. Same enforcement
# CI runs via golangci-lint, but local + fast (no v2-binary needed).
fmt-check:
	@diff=$$(gofmt -s -l . 2>&1); \
	if [ -n "$$diff" ]; then \
		echo "✘ gofmt -s drift in:"; echo "$$diff" | sed 's/^/    /'; \
		echo "  → run: gofmt -s -w ."; exit 1; \
	fi
	@echo "✔ gofmt -s clean"

tidy:
	go mod tidy

check: lint test build

# integration: everything `check` does PLUS the things that have actually
# broken pushes today. Designed as a pre-push smoke (~10s on a warm cache).
# Catches: gofmt drift, missing test, build break, broken catalog YAML,
# version-stamp regression, ghost CLI commands in docs.
#
# Use:
#   make integration
# CI runs the same surface plus golangci-lint v2 on every push, so a
# green `make integration` does not guarantee a green CI run — but it
# catches everything most likely to break.
integration: fmt-check lint test build
	@echo
	@echo "==== binary sanity ===="
	@./$(BIN) version
	@./$(BIN) help >/dev/null
	@./$(BIN) connect --list >/dev/null
	@./$(BIN) model search >/dev/null 2>&1 || true
	@echo "✔ binary smoke OK"
	@echo
	@echo "==== drift tests ===="
	@go test -run "TestDocs|TestCatalog|TestVersion" ./cmd/opod/ | tail -3
	@echo
	@echo "\033[1;32mINTEGRATION PASS\033[0m — safe to push."

run: build
	./$(BIN) up

clean:
	rm -f $(BIN)
	rm -rf data/ .opod/

.DEFAULT_GOAL := build
