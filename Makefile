GO ?= go
GOTOOLCHAIN ?= go1.26.9
export GOTOOLCHAIN
VERSION ?= 0.1.0

.PHONY: build test race vet check format

build:
	CGO_ENABLED=1 GOOS=darwin $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/obsidian-fin ./cmd/obsidian-fin

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

check: test race vet build

format:
	$(GO) fmt ./...
