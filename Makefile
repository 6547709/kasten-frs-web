GO ?= go
BIN := bin/helper
# Version is injected at build time; pass VERSION=vX.Y.Z to override
# the default "dev". Surfaced in the UI footer so the running binary
# is identifiable without checking the image tag.
VERSION ?= dev
LDFLAGS := -s -w -X 'main.version=$(VERSION)'

.PHONY: build test test-race test-cover lint fmt vet run clean fetch-htmx

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/helper

test:
	$(GO) test ./...

test-race:
	$(GO) test -race -shuffle=on ./...

test-cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

lint:
	@command -v golangci-lint >/dev/null 2>&1 || go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.59.1
	golangci-lint run ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

run: build
	./$(BIN)

clean:
	rm -f $(BIN) coverage.out

fetch-htmx:
	./scripts/fetch-htmx.sh
