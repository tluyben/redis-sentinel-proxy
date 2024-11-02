BINARY_NAME=redis-sentinel-proxy
build:
	go build -o $(BINARY_NAME) main.go
run:
	go run main.go
clean:
	go clean
	rm -f $(BINARY_NAME)
test:
	go test ./...
.PHONY: build run clean test
