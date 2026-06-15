GO ?= go
BIN := bin/helper
LDFLAGS := -s -w

.PHONY: build test test-race test-cover lint fmt vet run clean

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
