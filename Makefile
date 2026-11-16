VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GO_IMAGE := golang:1.26

.PHONY: build release dev test vet docker-release clean

# Binary for this machine.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/skiff ./cmd/skiff

# Static Linux binaries for servers.
release:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/skiff-linux-amd64 ./cmd/skiff
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/skiff-linux-arm64 ./cmd/skiff

# Run the panel locally without touching your machine: commands are printed, not run.
dev:
	go run ./cmd/skiff serve --dry-run --addr 127.0.0.1:8090 --data ./.data

test:
	go test ./...

vet:
	go vet ./...

# Build the Linux binaries without Go installed locally.
docker-release:
	docker run --rm -v "$(CURDIR)":/src -w /src -e VERSION=$(VERSION) $(GO_IMAGE) make release

clean:
	rm -rf dist .data
