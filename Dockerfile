# Build stage
FROM registry.access.redhat.com/ubi9/go-toolset:1.25 AS builder

WORKDIR /opt/app-root/src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -ldflags "-X main.version=${VERSION}" -o /opt/app-root/rhaii-validate-agent ./cmd/agent/

# Runtime stage
FROM registry.access.redhat.com/ubi9/ubi:latest

LABEL name="rhaii-validate-agent" \
      vendor="Red Hat" \
      summary="RHAII Cluster Validation Agent" \
      description="Per-node hardware validation agent for GPU, RDMA, and network checks"

# Install RDMA and networking tools
RUN dnf install -y \
      libibverbs-utils \
      infiniband-diags \
      iperf3 \
      pciutils \
      && dnf clean all

COPY --from=builder /opt/app-root/rhaii-validate-agent /usr/local/bin/rhaii-validate-agent

# nvidia-smi is expected to be available via host mount or GPU operator
# The DaemonSet spec should mount /usr/bin/nvidia-smi from the host

USER 0

ENTRYPOINT ["rhaii-validate-agent"]
