.PHONY: build test container container-rdma push push-rdma download install uninstall deploy deploy-all logs clean clean-all run-local fmt lint help

IMG ?= quay.io/opendatahub/odh-rhaii-cluster-validator:latest
export IMG
NAMESPACE ?= rhaii-validation
VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS := -X main.version=$(VERSION)

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
	@echo "rhaii-cluster-validation - GPU/RDMA cluster validation"
	@echo ""
	@echo "Quick Start:"
	@echo "  make download       - Download and install kubectl plugin from GHCR (Linux only)"
	@echo "  make install        - Build and install from source (Linux and macOS)"
	@echo "  kubectl rhaii-validate all          # Run all checks"
	@echo "  kubectl rhaii-validate gpu          # GPU checks only"
	@echo "  kubectl rhaii-validate network      # TCP bandwidth + latency tests"
	@echo "  kubectl rhaii-validate rdma         # All RDMA checks + connectivity + bandwidth"
	@echo "  kubectl rhaii-validate clean        # Cleanup"
	@echo ""
	@echo "  Or run directly via container (Linux and macOS):"
	@echo "    podman run --rm -it -v ~/.kube/config:/kubeconfig:z -e KUBECONFIG=/kubeconfig $(IMG) all"
	@echo ""
	@echo "Development:"
	@echo "  make build          - Build binary"
	@echo "  make test           - Run unit tests"
	@echo "  make lint           - Run linter"
	@echo "  make install        - Build + install as kubectl plugin"
	@echo "  make uninstall      - Remove kubectl plugin"
	@echo "  make container      - Build validator container image"
	@echo "  make container-rdma - Build tools container image"
	@echo "  make run-local      - Run checks locally (requires GPU node)"
	@echo ""
	@echo "Cleanup:"
	@echo "  make clean          - Remove validation resources (keep report)"
	@echo "  make clean-all      - Remove everything including report"

download:
	@# Container images contain Linux binaries — cannot run on macOS directly
	@if [ "$$(uname)" = "Darwin" ]; then \
		echo "ERROR: 'make download' extracts a Linux binary from the container image."; \
		echo "       On macOS, use 'make install' to build from source instead."; \
		exit 1; \
	fi
	@# Check for existing installs that could shadow the new one
	@EXISTING=$$(which kubectl-rhaii_validate 2>/dev/null); \
	if [ -n "$$EXISTING" ]; then \
		echo "WARNING: existing plugin found at $$EXISTING — removing it first"; \
		sudo rm -f "$$EXISTING"; \
	fi
	@echo "Downloading kubectl plugin from container image..."
	$(CONTAINER_RUNTIME) pull $(IMG)
	$(CONTAINER_RUNTIME) rm -f rhaii-extract >/dev/null 2>&1 || true
	$(CONTAINER_RUNTIME) create --name rhaii-extract "$(IMG)" >/dev/null
	$(CONTAINER_RUNTIME) cp rhaii-extract:/usr/local/bin/rhaii-validator kubectl-rhaii_validate; \
		$(CONTAINER_RUNTIME) rm -f rhaii-extract >/dev/null 2>&1 || true
	chmod +x kubectl-rhaii_validate
	sudo mv kubectl-rhaii_validate /usr/local/bin/
	@echo "Installed! Run: kubectl rhaii-validate all"
	@kubectl rhaii-validate --version

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/rhaii-validator ./cmd/agent/

install: build
	@# Check for existing installs that could shadow the new one
	@EXISTING=$$(which kubectl-rhaii_validate 2>/dev/null); \
	if [ -n "$$EXISTING" ]; then \
		echo "WARNING: existing plugin found at $$EXISTING — removing it first"; \
		sudo rm -f "$$EXISTING"; \
	fi
	@echo "Installing kubectl plugin..."
	cp bin/rhaii-validator /usr/local/bin/kubectl-rhaii_validate 2>/dev/null || \
		cp bin/rhaii-validator $(HOME)/.local/bin/kubectl-rhaii_validate
	@echo "Installed! Run: kubectl rhaii-validate all"
	@kubectl rhaii-validate --version

uninstall:
	rm -f /usr/local/bin/kubectl-rhaii_validate $(HOME)/.local/bin/kubectl-rhaii_validate
	@echo "kubectl plugin removed"

test:
	go test ./... -v

IMG_TOOLS ?= quay.io/opendatahub/odh-rhaii-validator-tools:latest
RDMA_BUILDER_IMAGE ?= nvcr.io/nvidia/cuda:13.0.0-devel-ubi9
RDMA_RUNTIME_IMAGE ?= registry.redhat.io/ubi9/ubi:latest
container:
	$(CONTAINER_RUNTIME) build -f Dockerfile.dev --platform $(TARGET_PLATFORM) --build-arg VERSION=$(VERSION) -t $(IMG) .

container-rdma:
	$(CONTAINER_RUNTIME) build -f tools/Dockerfile.dev \
		--build-arg BUILDER_IMAGE=$(RDMA_BUILDER_IMAGE) \
		--build-arg RUNTIME_IMAGE=$(RDMA_RUNTIME_IMAGE) \
		-t $(IMG_TOOLS) .

push:
	$(CONTAINER_RUNTIME) push $(IMG)

push-rdma:
	$(CONTAINER_RUNTIME) push $(IMG_TOOLS)

deploy: install
	kubectl rhaii-validate all

deploy-all: build container push deploy

logs:
	@echo "=== GPU Check Job Results ==="
	@for pod in $$(kubectl get pods -n "$(NAMESPACE)" -l app=rhaii-validate-gpu-check -o jsonpath='{.items[*].metadata.name}'); do \
		echo "--- $$pod ---"; \
		kubectl logs -n "$(NAMESPACE)" "$$pod" 2>/dev/null; \
		echo ""; \
	done
	@echo "=== RDMA Node Check Job Results ==="
	@for pod in $$(kubectl get pods -n "$(NAMESPACE)" -l app=rhaii-validate-net-check -o jsonpath='{.items[*].metadata.name}'); do \
		echo "--- $$pod ---"; \
		kubectl logs -n "$(NAMESPACE)" "$$pod" 2>/dev/null; \
		echo ""; \
	done
	@echo "=== BW Probe Job Results ==="
	@for pod in $$(kubectl get pods -n "$(NAMESPACE)" -l app=rhaii-validate-bw-probe -o jsonpath='{.items[*].metadata.name}'); do \
		echo "--- $$pod ---"; \
		kubectl logs -n "$(NAMESPACE)" "$$pod" 2>/dev/null; \
		echo ""; \
	done

clean:
	@echo "Cleaning up validation resources (preserving ConfigMap)..."
	-kubectl delete jobs -n "$(NAMESPACE)" -l app=rhaii-validate-gpu-check --ignore-not-found
	-kubectl delete jobs -n "$(NAMESPACE)" -l app=rhaii-validate-net-check --ignore-not-found
	-kubectl delete jobs -n "$(NAMESPACE)" -l app=rhaii-validate-bw-probe --ignore-not-found
	-kubectl delete jobs -n "$(NAMESPACE)" -l app=rhaii-validate-job --ignore-not-found
	-kubectl delete jobs -n "$(NAMESPACE)" -l rhaii-job-type=pingmesh --ignore-not-found
	-kubectl delete serviceaccount rhaii-validator -n "$(NAMESPACE)" --ignore-not-found
	-kubectl delete clusterrolebinding rhaii-validator --ignore-not-found
	-kubectl delete clusterrolebinding rhaii-validator-scc --ignore-not-found
	-kubectl delete clusterrole rhaii-validator --ignore-not-found
	@echo "ConfigMaps preserved: rhaii-validate-config (user config), rhaii-validate-report (results)"

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
