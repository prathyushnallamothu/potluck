BINARY  := potluck
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: build run test vet clean release

build:
	go build $(LDFLAGS) -o bin/$(BINARY) ./cmd/potluck

run: build
	./bin/$(BINARY)

test: vet
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin dist

# Cross-compile release binaries. CGO is required on darwin for gopsutil's
# CPU info, so darwin builds must run on a Mac.
release:
	mkdir -p dist
	GOOS=darwin  GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BINARY)-darwin-arm64 ./cmd/potluck
	GOOS=darwin  GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BINARY)-darwin-amd64 ./cmd/potluck
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BINARY)-linux-amd64 ./cmd/potluck
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BINARY)-linux-arm64 ./cmd/potluck
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BINARY)-windows-amd64.exe ./cmd/potluck
