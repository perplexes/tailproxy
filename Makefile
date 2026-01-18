.PHONY: build clean install test help

BINARY_NAME=tailproxy
LIB_NAME=libtailproxy.so
INSTALL_PATH=/usr/local/bin
INSTALL_LIB_PATH=/usr/local/lib

help:
	@echo "Available targets:"
	@echo "  build       - Build tailproxy binary and preload library"
	@echo "  clean       - Remove built binaries and cache"
	@echo "  install     - Install tailproxy to $(INSTALL_PATH)"
	@echo "  uninstall   - Remove tailproxy from $(INSTALL_PATH)"
	@echo "  test        - Run tests"
	@echo "  help        - Show this help message"

build: $(LIB_NAME) $(BINARY_NAME)

$(LIB_NAME): preload.c
	@echo "Building $(LIB_NAME)..."
	@gcc -shared -fPIC -O2 -Wall -o $(LIB_NAME) preload.c -ldl
	@echo "Build complete: $(LIB_NAME)"

$(BINARY_NAME): $(LIB_NAME) *.go
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p .build
	@TMPDIR=.build GOCACHE=.build/cache go build -o $(BINARY_NAME) main.go config.go proxy.go
	@echo "Build complete: $(BINARY_NAME)"

clean:
	@echo "Cleaning up..."
	@rm -f $(BINARY_NAME) $(LIB_NAME)
	@rm -rf .build
	@go clean -cache -testcache
	@echo "Clean complete"

install: build
	@echo "Installing $(BINARY_NAME) and $(LIB_NAME) to $(INSTALL_PATH)..."
	@sudo cp $(BINARY_NAME) $(INSTALL_PATH)/
	@sudo cp $(LIB_NAME) $(INSTALL_PATH)/
	@sudo chmod +x $(INSTALL_PATH)/$(BINARY_NAME)
	@sudo chmod 644 $(INSTALL_PATH)/$(LIB_NAME)
	@echo "Installation complete"

uninstall:
	@echo "Removing $(BINARY_NAME) and $(LIB_NAME) from $(INSTALL_PATH)..."
	@sudo rm -f $(INSTALL_PATH)/$(BINARY_NAME)
	@sudo rm -f $(INSTALL_PATH)/$(LIB_NAME)
	@echo "Uninstallation complete"

test:
	@echo "Running tests..."
	@go test -v ./...

.DEFAULT_GOAL := build
