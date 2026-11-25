.PHONY: all run build check lint test test-large format release docs

VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty))

all: check

run:
	go run .

build:
	go build -trimpath -ldflags "-X main.version=$(VERSION)" -o dist/loka .

check: lint test

# This needs golangci-lint v2. See
# https://golangci-lint.run/docs/welcome/install/
lint:
	golangci-lint run

test:
	go test -race ./...

# Sends a file over 8 GiB, which needs that much free disk space.
test-large:
	LOKA_LARGE_TESTS=1 go test -count=1 -run Over8GiB ./internal/transfer/

format:
	gofmt -w .

release:
	scripts/build-release.sh dist

# Serves the website in docs/ at http://127.0.0.1:8000
docs:
	python3 -m http.server -b 127.0.0.1 -d docs 8000
