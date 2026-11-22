.PHONY: build test check run-http

build:
	go build -o bin/mcp-books ./src

test:
	go test ./src -v

check: build
	./bin/mcp-books -check

run-http: build
	./bin/mcp-books -http 127.0.0.1:8090
