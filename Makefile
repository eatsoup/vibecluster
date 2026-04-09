BINARY := vibecluster
BUILD_DIR := bin
GO := go
SYNCER_IMAGE := ghcr.io/eatsoup/vibecluster/syncer:latest

.PHONY: build clean install test syncer-image syncer-load

build:
	$(GO) build -o $(BUILD_DIR)/$(BINARY) ./cmd/vibecluster/

build-syncer:
	$(GO) build -o $(BUILD_DIR)/syncer ./cmd/syncer/

install: build
	cp $(BUILD_DIR)/$(BINARY) $(GOPATH)/bin/$(BINARY) 2>/dev/null || cp $(BUILD_DIR)/$(BINARY) /usr/local/bin/$(BINARY)

clean:
	rm -rf $(BUILD_DIR)

test:
	$(GO) test ./...

syncer-image:
	docker build -f Dockerfile.syncer -t $(SYNCER_IMAGE) .

syncer-push: syncer-image
	docker push $(SYNCER_IMAGE)

all: build syncer-image
