BINARY := vibecluster
BUILD_DIR := bin
GO := go
SYNCER_IMAGE := ghcr.io/eatsoup/vibecluster/syncer:latest
OPERATOR_IMAGE := ghcr.io/eatsoup/vibecluster/operator:latest

.PHONY: build clean install test syncer-image syncer-load build-operator operator-image operator-push install-crd deploy-operator

build:
	$(GO) build -o $(BUILD_DIR)/$(BINARY) ./cmd/vibecluster/

build-syncer:
	$(GO) build -o $(BUILD_DIR)/syncer ./cmd/syncer/

build-operator:
	$(GO) build -o $(BUILD_DIR)/operator ./cmd/operator/

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

operator-image:
	docker build -f Dockerfile.operator -t $(OPERATOR_IMAGE) .

operator-push: operator-image
	docker push $(OPERATOR_IMAGE)

install-crd:
	kubectl apply -f config/crd/vibecluster.dev_virtualclusters.yaml

deploy-operator: install-crd
	kubectl apply -k config/operator/

undeploy-operator:
	kubectl delete -k config/operator/ --ignore-not-found

all: build syncer-image operator-image
