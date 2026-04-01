GO ?= /usr/local/go/bin/go
BINARY = rclone

.PHONY: build clean test tidy release-snapshot release

# Build with FUSE mount support (requires macFUSE on macOS, libfuse on Linux)
build:
	$(GO) build -tags cmount -o $(BINARY) .

# Build without FUSE (portable, all platforms)
build-portable:
	CGO_ENABLED=0 $(GO) build -o $(BINARY) .

clean:
	rm -f $(BINARY)
	rm -rf dist/

test:
	$(GO) test ./backend/fusiondata/ -v

tidy:
	$(GO) mod tidy

# Test release locally without publishing
release-snapshot:
	goreleaser release --snapshot --clean

# Full release (requires a git tag, run by CI)
release:
	goreleaser release --clean
