BIN := dist/srcproof

.PHONY: all build test fmt clean

all: test

build:
	go build -o $(BIN) ./cmd/srcproof

test:
	go vet ./...
	go test ./...

fmt:
	go fmt ./...

clean:
	rm -rf dist
