BINARY := irodsfsd
VERSION ?= dev
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X github.com/cyverse/irodsfsd/internal/command.version=$(VERSION) \
	-X github.com/cyverse/irodsfsd/internal/command.gitCommit=$(GIT_COMMIT)

.PHONY: build test clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/irodsfsd

test:
	go test ./...

clean:
	rm -f bin/$(BINARY)
