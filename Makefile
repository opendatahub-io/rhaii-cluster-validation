.PHONY: build test container push deploy run logs clean help

IMG ?= quay.io/opendatahub/rhaii-validate-agent:latest
export IMG
NAMESPACE ?= rhaii-validation
VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS := -X main.version=$(VERSION)

help:
	@echo "rhaii-cluster-validation - GPU/RDMA validation agent"
	@echo ""
	@echo "Build:"
	@echo "  make build          - Build agent binary"
	@echo "  make test           - Run unit tests"
	@echo "  make container      - Build container image"
	@echo "  make push           - Push container image"
	@echo ""
	@echo "Deploy:"
	@echo "  make deploy         - Full lifecycle: deploy agents, collect, report, cleanup (IMG=...)"
	@echo "  make run            - Deploy agent DaemonSet only via kubectl (IMG=...)"
	@echo "  make logs           - Collect agent results from pod logs"
	@echo "  make clean          - Remove agent DaemonSet"
	@echo ""
	@echo "CLI Testing:"
	@echo "  make run-local      - Run agent locally (requires GPU node)"

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/rhaii-validate-agent ./cmd/agent/

test:
	go test ./... -v

container:
	podman build --build-arg VERSION=$(VERSION) -t $(IMG) .

push:
	podman push $(IMG)

deploy: build
	./bin/rhaii-validate-agent deploy --image $(IMG)

run:
	kubectl apply -f deploy/rbac.yaml
	envsubst < deploy/daemonset.yaml | kubectl apply -f -
	@echo "Waiting for agent pods to start..."
	kubectl rollout status daemonset/rhaii-validate-agent -n $(NAMESPACE) --timeout=120s

logs:
	@echo "=== Agent Results ==="
	@for pod in $$(kubectl get pods -n $(NAMESPACE) -l app=rhaii-validate-agent -o jsonpath='{.items[*].metadata.name}'); do \
		echo "--- $$pod ---"; \
		kubectl logs -n $(NAMESPACE) $$pod 2>/dev/null; \
		echo ""; \
	done

clean:
	-kubectl delete daemonset rhaii-validate-agent -n $(NAMESPACE) --ignore-not-found
	-kubectl delete jobs -n $(NAMESPACE) -l app=rhaii-validate-job --ignore-not-found
	-kubectl delete -f deploy/rbac.yaml --ignore-not-found

run-local:
	@echo "Running agent locally on this node..."
	go run ./cmd/agent/ run --no-wait --node-name $$(hostname)

fmt:
	go fmt ./...

lint:
	golangci-lint run ./...
