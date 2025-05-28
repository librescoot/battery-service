.PHONY: build clean build-arm build-amd64 lint test

BINARY_NAME=battery-service
BUILD_DIR=bin
LDFLAGS=-ldflags "-w -s -extldflags '-static'"
CMD_DIR=cmd/battery-service

build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./$(CMD_DIR)

clean:
	rm -rf $(BUILD_DIR)

build-arm: build

build-amd64:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-amd64 ./$(CMD_DIR)

lint:
	golangci-lint run

test:
	go test -v ./...

# Additional targets for development
run:
	go run ./$(CMD_DIR)

dev-build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./$(CMD_DIR)

# Build for the current platform (useful for testing)
build-native:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./$(CMD_DIR) 