GO ?= /usr/local/go/bin/go
BINARY = rclone

.PHONY: build clean test

build:
	$(GO) build -tags cmount -o $(BINARY) .

clean:
	rm -f $(BINARY)

test:
	$(GO) test ./backend/fusiondata/ -v

tidy:
	$(GO) mod tidy
