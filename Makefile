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

# Signed WASM loops.
#
# Each loop is a separate module compiled for wasip1 and signed with this
# machine's publisher key. `make loops` builds and signs every one; the
# artifacts are what `karmax wloop install` takes.
LOOPS := chat-sweep cold-scan gchat-watch wa-monitor
LOOP_OUT ?= dist/loops

.PHONY: loops loops-clean
loops: $(LOOP_OUT)
	@for l in $(LOOPS); do \
		echo "building $$l"; \
		GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared \
			-o $(LOOP_OUT)/$$l.wasm ./loops/$$l || exit 1; \
		go run ./cmd/karmax wloop sign \
			--manifest loops/$$l/loop.yaml \
			--module $(LOOP_OUT)/$$l.wasm \
			-o $(LOOP_OUT)/$$l.kloop || exit 1; \
	done
	@echo
	@echo "Signed artifacts are in $(LOOP_OUT). Install one with:"
	@echo "  karmax wloop install $(LOOP_OUT)/<name>.kloop"

$(LOOP_OUT):
	@mkdir -p $(LOOP_OUT)

loops-clean:
	@rm -rf $(LOOP_OUT)
