GO ?= go
DIST := dist
TARGETS := darwin/arm64 darwin/amd64 linux/arm64 linux/amd64

.PHONY: test build release clean

test:
	$(GO) test ./...
	$(GO) vet ./...

build:
	mkdir -p bin
	$(GO) build -trimpath -o bin/pi-bun .

release:
	mkdir -p $(DIST)
	@for target in $(TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' -o $(DIST)/pi-bun-$$os-$$arch .; \
	done

clean:
	rm -rf bin $(DIST)
