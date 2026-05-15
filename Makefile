VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/CarriedWorldUniverse/bridle/internal/version.Version=$(VERSION)

.PHONY: build test vet stubfunnel version clean

build:
	go build ./...

stubfunnel:
	go build -ldflags '$(LDFLAGS)' -o bin/stubfunnel ./stubfunnel

test:
	go test -race ./...

vet:
	go vet ./...

version:
	@echo $(VERSION)

clean:
	rm -rf bin/
