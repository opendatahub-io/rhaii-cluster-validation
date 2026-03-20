.PHONY: build test container container-rdma container-rdma-dev push push-rdma push-rdma-dev deploy deploy-all install uninstall logs clean clean-all help

IMG ?= quay.io/opendatahub/rhaii-validator:latest
export IMG
NAMESPACE ?= rhaii-validation
VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS := -X main.version=$(VERSION) -X main.defaultImage=$(IMG)

# Detect container runtime (podman, docker, or flatpak-spawn --host podman)
CONTAINER_RUNTIME ?= $(shell \
	if command -v podman >/dev/null 2>&1; then \
		echo "podman"; \
	elif command -v docker >/dev/null 2>&1; then \
		echo "docker"; \
	elif command -v flatpak-spawn >/dev/null 2>&1; then \
		echo "flatpak-spawn --host podman"; \
	else \
		echo "echo 'Error: no container runtime found. Install podman or docker.' && exit 1"; \
	fi)

# Target platform for container images
TARGET_PLATFORM ?= linux/amd64

help:
	@echo "rhaii-cluster-validation - GPU/RDMA validation agent"
	@echo ""
	@echo "Build:"
	@echo "  make build          - Build agent binary"
	@echo "  make test           - Run unit tests"
	@echo "  make container      - Build container image (linux/amd64)"
	@echo "  make container-rdma - Build RDMA tools image (aipcc base)"
	@echo "  make container-rdma-dev - Build RDMA tools dev image (nvcr.io base)"
	@echo "  make push           - Push container image"
	@echo "  make install        - Install as kubectl plugin (kubectl rhaii-validate)"
	@echo "  make uninstall      - Remove kubectl plugin"
	@echo ""
	@echo "Deploy:"
	@echo "  make deploy         - Deploy using existing image (IMG=...)"
	@echo "  make deploy-all     - Build + push + deploy (IMG=...)"
	@echo "  make logs           - Collect check job results from pod logs"
	@echo "  make clean          - Remove check jobs, bandwidth jobs, and RBAC"
	@echo "  make clean-all      - Remove all resources including ConfigMap"
	@echo ""
	@echo "CLI Testing:"
	@echo "  make run-local      - Run agent locally (requires GPU node)"

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/rhaii-validator ./cmd/agent/

install: build
	@echo "Installing kubectl plugin..."
	cp bin/rhaii-validator /usr/local/bin/kubectl-rhaii_validate 2>/dev/null || \
		cp bin/rhaii-validator $(HOME)/.local/bin/kubectl-rhaii_validate
	@echo "Installed! Usage: kubectl rhaii-validate deploy --image $(IMG)"
	@echo "Verify:  kubectl plugin list | grep rhaii"

uninstall:
	rm -f /usr/local/bin/kubectl-rhaii_validate $(HOME)/.local/bin/kubectl-rhaii_validate
	@echo "kubectl plugin removed"

test:
	go test ./... -v

IMG_TOOLS ?= quay.io/opendatahub/rhaii-rdma-tools:latest
IMG_TOOLS_DEV ?= quay.io/opendatahub/rhaii-rdma-tools-dev:latest
RDMA_BUILDER_IMAGE ?= quay.io/aipcc/base-images/cuda-13.0-el9.6:3.4
RDMA_RUNTIME_IMAGE ?= quay.io/aipcc/base-images/cuda-13.0-el9.6:3.4

container:
	$(CONTAINER_RUNTIME) build --platform $(TARGET_PLATFORM) --build-arg VERSION=$(VERSION) -t $(IMG) .

container-rdma:
	$(CONTAINER_RUNTIME) build -f Dockerfile.rdma-tools \
		--build-arg BUILDER_IMAGE=$(RDMA_BUILDER_IMAGE) \
		--build-arg RUNTIME_IMAGE=$(RDMA_RUNTIME_IMAGE) \
		-t $(IMG_TOOLS) .

container-rdma-dev:
	$(CONTAINER_RUNTIME) build -f Dockerfile.rdma-tools \
		--build-arg BUILDER_IMAGE=nvcr.io/nvidia/cuda:13.0.0-devel-ubi9 \
		--build-arg RUNTIME_IMAGE=nvcr.io/nvidia/cuda:13.0.0-runtime-ubi9 \
		-t $(IMG_TOOLS_DEV) .

push:
	$(CONTAINER_RUNTIME) push $(IMG)

push-rdma:
	$(CONTAINER_RUNTIME) push $(IMG_TOOLS)

push-rdma-dev:
	$(CONTAINER_RUNTIME) push $(IMG_TOOLS_DEV)

deploy: install
	kubectl rhaii-validate all

deploy-all: build container push deploy

logs:
	@echo "=== Check Job Results ==="
	@for pod in $$(kubectl get pods -n $(NAMESPACE) -l app=rhaii-validate-check -o jsonpath='{.items[*].metadata.name}'); do \
		echo "--- $$pod ---"; \
		kubectl logs -n $(NAMESPACE) $$pod 2>/dev/null; \
		echo ""; \
	done

clean:
	@echo "Cleaning up validation resources (preserving ConfigMap)..."
	-kubectl delete jobs -n $(NAMESPACE) -l app=rhaii-validate-check --ignore-not-found
	-kubectl delete jobs -n $(NAMESPACE) -l app=rhaii-validate-job --ignore-not-found
	-kubectl delete serviceaccount rhaii-validator -n $(NAMESPACE) --ignore-not-found
	-kubectl delete clusterrolebinding rhaii-validator --ignore-not-found
	-kubectl delete clusterrole rhaii-validator --ignore-not-found
	@echo "ConfigMap preserved: kubectl get cm rhaii-validate-config -n $(NAMESPACE)"

clean-all: clean
	@echo "Removing ConfigMap and namespace..."
	-kubectl delete configmap rhaii-validate-config -n $(NAMESPACE) --ignore-not-found
	-kubectl delete namespace $(NAMESPACE) --ignore-not-found
	@echo "Full cleanup complete"

run-local:
	@echo "Running agent locally on this node..."
	go run ./cmd/agent/ run --node-name $$(hostname)

fmt:
	go fmt ./...

lint:
	golangci-lint run ./...
