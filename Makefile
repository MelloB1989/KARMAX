.PHONY: build run test lint clean install

BINARY := karmax
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.Version=$(VERSION)"

build:
	CGO_ENABLED=1 go build $(LDFLAGS) -o $(BINARY) ./cmd/karmax

run:
	./$(BINARY) start

test:
	go test ./... -race -count=1

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY)

install:
	CGO_ENABLED=1 go install $(LDFLAGS) ./cmd/karmax

build-nocgo:
	CGO_ENABLED=0 go build -tags "modernc" $(LDFLAGS) -o $(BINARY) ./cmd/karmax
