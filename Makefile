BINARY  := salvage
PKG     := salvage.sh/cmd/salvage
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X salvage.sh/internal/version.Version=$(VERSION) \
	-X salvage.sh/internal/version.Commit=$(COMMIT) \
	-X salvage.sh/internal/version.Date=$(DATE)

.PHONY: build test vet fmt run install tidy clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/salvage

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

run: build
	./$(BINARY) run -config salvage.yaml

install:
	go install -trimpath -ldflags "$(LDFLAGS)" $(PKG)

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)
	rm -rf dist
