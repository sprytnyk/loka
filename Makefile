.PHONY: all run build check lint test test-large format release

VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty))

all: check

run:
	go run .

build:
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o dist/loka .

check: lint test

lint:
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...

test:
	go test -race ./...

# Sends a file over 8 GiB, which needs that much free disk space.
test-large:
	LOKA_LARGE_TESTS=1 go test -count=1 -run Over8GiB ./internal/transfer/

format:
	gofmt -w .

release:
	scripts/build-release.sh dist
