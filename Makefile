MODULE  := github.com/agent-burn-down/desktop-client
BINARY  := burndown-cli
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

export CGO_ENABLED=0

.PHONY: build build-all test lint snapshot clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

build-all:
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-darwin-arm64 ./cmd/$(BINARY)
	GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-darwin-amd64 ./cmd/$(BINARY)

test:
	go test -race -cover ./...

lint:
	golangci-lint run ./...

snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist
