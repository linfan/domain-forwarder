.PHONY: all build clean proxy request-printer test run-proxy run-printer

# Build output directory
BUILD_DIR := build

# Go build flags for optimization
LDFLAGS := -s -w
GCFLAGS := -B

all: build

build: proxy request-printer

proxy:
	@mkdir -p $(BUILD_DIR)
	go build -ldflags="$(LDFLAGS)" -gcflags="$(GCFLAGS)" -o $(BUILD_DIR)/proxy .

request-printer:
	@mkdir -p $(BUILD_DIR)
	go build -ldflags="$(LDFLAGS)" -gcflags="$(GCFLAGS)" -o $(BUILD_DIR)/request-printer ./tools/request-printer

clean:
	rm -rf $(BUILD_DIR)
	rm -f proxy request-printer
	rm -f tools/request-printer/request-printer

# Run targets
run-proxy: proxy
	$(BUILD_DIR)/proxy -addr :8080

run-printer: request-printer
	$(BUILD_DIR)/request-printer -addr :9090

# Test the proxy with request-printer
test: build
	@echo "Starting request-printer on :9090..."
	@$(BUILD_DIR)/request-printer -addr :9090 & echo $$! > /tmp/printer.pid
	@sleep 1
	@echo "Starting proxy on :8080..."
	@$(BUILD_DIR)/proxy -addr :8080 & echo $$! > /tmp/proxy.pid
	@sleep 1
	@echo "\n=== Testing HTTP proxy ==="
	@curl -s -x http://localhost:8080 'http://localhost:9090/test?foo=bar' | head -20
	@echo "\n=== Testing HTTPS proxy ==="
	@curl -s -x http://localhost:8080 'https://httpbin.org/get' | head -10
	@echo "\n=== Stopping services ==="
	@kill `cat /tmp/proxy.pid` 2>/dev/null || true
	@kill `cat /tmp/printer.pid` 2>/dev/null || true
	@rm -f /tmp/proxy.pid /tmp/printer.pid
	@echo "Test completed."

# Install to GOPATH/bin
install: build
	cp $(BUILD_DIR)/proxy $(GOPATH)/bin/tiny-http-proxy
	cp $(BUILD_DIR)/request-printer $(GOPATH)/bin/request-printer
