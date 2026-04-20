BINARY := vibecluster
BUILD_DIR := bin
GO := go
SYNCER_IMAGE := ghcr.io/eatsoup/vibecluster/syncer:latest
OPERATOR_IMAGE := ghcr.io/eatsoup/vibecluster/operator:latest

.PHONY: build clean install test test-e2e syncer-image syncer-load build-operator operator-image operator-push install-crd deploy-operator

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

# test-e2e spins up a k3d cluster, loads locally-built images, and runs the
# full integration suite. Requires k3d and kubectl in PATH.
# Override images and binary with env vars:
#   VIBC_SYNCER_IMAGE   – syncer image to load (default: build locally)
#   VIBC_OPERATOR_IMAGE – operator image to load (default: build locally)
#   VIBC_HOST_KUBECONFIG – skip cluster creation and use this kubeconfig
#   VIBC_KEEP_CLUSTER   – set to any value to leave the k3d cluster after tests
E2E_SYNCER_IMAGE  ?= ghcr.io/eatsoup/vibecluster/syncer:e2e
E2E_OPERATOR_IMAGE ?= ghcr.io/eatsoup/vibecluster/operator:e2e

test-e2e: build
	@which k3d   >/dev/null 2>&1 || (echo "ERROR: k3d not found; install from https://k3d.io" && exit 1)
	@which kubectl >/dev/null 2>&1 || (echo "ERROR: kubectl not found" && exit 1)
	docker build -f Dockerfile.syncer  -t $(E2E_SYNCER_IMAGE)  .
	docker build -f Dockerfile.operator -t $(E2E_OPERATOR_IMAGE) .
	VIBC_BIN=$(PWD)/$(BUILD_DIR)/$(BINARY) \
	  VIBC_SYNCER_IMAGE=$(E2E_SYNCER_IMAGE) \
	  VIBC_OPERATOR_IMAGE=$(E2E_OPERATOR_IMAGE) \
	  $(GO) test -v -tags e2e -timeout 45m ./test/e2e/...

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
