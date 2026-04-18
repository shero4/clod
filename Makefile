BINARY   := clod
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: build release clean test vet tidy

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

vet:
	go vet ./...

test: vet
	go build ./...

tidy:
	go mod tidy

release:
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "  building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-$$os-$$arch . || exit 1; \
	done
	cd dist && sha256sum * > SHA256SUMS
	@echo "done: $(VERSION)"

clean:
	rm -rf dist $(BINARY)
