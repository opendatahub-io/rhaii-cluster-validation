# EFA Support for Cluster Validation

Status: **Plan Finalized**

This document tracks the design and implementation of RDMA reachability and bandwidth checks for AWS EFA (Elastic Fabric Adapter) networks.

---

## Background

The validator currently supports two RDMA network types:

| | RoCEv2 | InfiniBand |
|---|---|---|
| **Reachability** | ICMP `ping` (RoCE uses IP/UDP) | `ibv_rc_pingpong` (IB data plane) |
| **Bandwidth** | `ib_write_bw --use_cuda` (RDMA WRITE, GPUDirect) | `ib_write_bw --use_cuda` (RDMA WRITE, GPUDirect) |

EFA uses the **Scalable Reliable Datagram (SRD)** protocol — an OS-bypass, kernel-bypass transport built into the EFA hardware. It is not IP-routable. The libfabric library provides the userspace interface (`fi_*` API).

For prefill-decode disaggregation with GPUDirect RDMA on EFA, NIXL uses the **libfabric backend** issuing `fi_read` (1-sided RDMA READ) operations. Our bandwidth test closely emulates this by issuing `fi_write` (1-sided RDMA WRITE) operations with GPU memory buffers.

---

## Tools

| | `fi_rdm_pingpong` | `fi_rma_bw` |
|---|---|---|
| **Purpose** | EFA/SRD reachability (pingmesh) | EFA bandwidth with GPUDirect RDMA |
| **What it tests** | SRD datapath connectivity + latency | 1-sided RMA WRITE throughput to GPU memory |
| **Source** | fabtests (standard, all versions) | fabtests (standard, all versions) |
| **Needs CUDA** | No | Yes (`-D cuda`) |
| **Needs OOB exchange** | Yes (`-E`) | Yes (`-E`) |
| **Server command** | `fi_rdm_pingpong -p efa -E` | `fi_rma_bw -p efa -o write -E -D cuda -S all` |
| **Client command** | `fi_rdm_pingpong -p efa -E <server_pod_ip>` | `fi_rma_bw -p efa -o write -E -D cuda -S all <server_pod_ip>` |
| **Required env vars** | `FI_PROVIDER=efa` | `FI_PROVIDER=efa`, `FI_EFA_USE_DEVICE_RDMA=1` |

`fi_rma_bw` has been [demonstrated](https://le.qun.ch/en/blog/2024/12/25/libfabric-efa-3-fi_info/) at 11,843 MB/sec (94.75 Gbps) on a single 100 Gbps EFA link — near line-rate with GPUDirect RDMA.

### How OOB Address Exchange Works (`-E` flag)

EFA endpoints use proprietary raw addresses (GID + QPN + ConnID, 32 bytes) that cannot be resolved from IP alone. The `-E` flag causes fabtests to:

1. Open a TCP socket between client and server using the **pod's primary IP** (eth0, from VPC CNI).
2. Each side calls `fi_getname()` to get its local EFA raw address.
3. They exchange raw addresses over the TCP socket.
4. Each side calls `fi_av_insert()` to add the peer's address to its address vector.
5. SRD traffic then flows directly over the EFA devices.

Pods always have their primary eth0 IP for the OOB channel, even when EFA-only interfaces (no IPs) are used.

---

## Version Pinning

All component versions are pinned to match **AWS EFA installer 1.46.0**, which is the version used by the [llm-d v0.8.1 AWS EFA image](https://github.com/llm-d/llm-d/pull/607).

| Component | Version | Source | Tag |
|---|---|---|---|
| rdma-core | 60.0 | `github.com/linux-rdma/rdma-core` | `v60.0` |
| libfabric | 2.3.1amzn4.0 | `github.com/aws/libfabric` | `v2.3.1amzn4.0` |
| fabtests | (bundled in libfabric tarball) | `github.com/aws/libfabric` | `v2.3.1amzn4.0` |
| perftest | (existing submodule) | already in repo | — |

### Why these versions

- **rdma-core 60.0**: Upstream `linux-rdma/rdma-core`. EFA installer 1.45.0 upgraded to this version; 1.46.0 carried it forward unchanged. Provides `libibverbs.so`, `libefa.so` (EFA verbs provider), `libmlx5.so` (IB/RoCE verbs provider), and `librdmacm.so`.
- **libfabric 2.3.1amzn4.0**: From `aws/libfabric`, which is a [snapshot of upstream `ofiwg/libfabric` v2.3.x branch](https://github.com/aws/libfabric/releases/tag/v2.3.1amzn4.0) commit `e33a074`. The `aws/libfabric` repo contains only CI/release workflows — all source code is upstream.
- **rdma-core is pure upstream**: The EFA changelog explicitly links to `github.com/linux-rdma/rdma-core/releases/tag/v60.0`. No AWS fork exists.

### Software stack

```
fi_rma_bw / fi_rdm_pingpong (fabtests binaries)
    → libfabric.so.1 (EFA provider compiled in, CUDA HMEM enabled)
        → libibverbs.so.1 (from rdma-core 60.0)
            → libefa.so.1 (ibverbs hardware provider for EFA, from rdma-core 60.0)
                → EFA kernel driver (efa.ko, on host node)

ib_write_bw (perftest binary)
    → libibverbs.so.1 (from rdma-core 60.0)
        → libmlx5.so.1 (ibverbs hardware provider for IB/RoCE, from rdma-core 60.0)
            → mlx5 kernel driver (on host node)
```

Both tool sets share the **same rdma-core build** — no ABI conflicts.

---

## Image Strategy

**Decision: Extend the existing tools image** (single composite image for IB/RoCE + EFA).

### Build order in Dockerfile

```
1. rdma-core 60.0 (cmake)    → libibverbs.so, libefa.so, libmlx5.so, librdmacm.so
2. libfabric 2.3.1amzn4.0    → libfabric.so (--with-cuda, linked to rdma-core from step 1)
3. fabtests (from libfabric)  → fi_rdm_pingpong, fi_rma_bw (--with-cuda --with-libfabric)
4. perftest (existing)        → ib_write_bw, ibv_rc_pingpong (--enable-cudart, linked to rdma-core)
```

### Konflux compatibility

Both `github.com/linux-rdma/rdma-core` and `github.com/aws/libfabric` are public GitHub repos with tagged releases. The Konflux build (no general internet, but GitHub + registry.redhat.io accessible) can fetch source tarballs from these tags.

---

## Pod / Container Requirements

```yaml
resources:
  limits:
    nvidia.com/gpu: "1"
    vpc.amazonaws.com/efa: "4"
  requests:
    vpc.amazonaws.com/efa: "4"
securityContext:
  capabilities:
    add:
      - IPC_LOCK
```

- **`IPC_LOCK` capability**: Required for libfabric to register memory (CUDA VRAM or host) with the EFA NIC via `fi_mr_regattr()`. This is what the [llm-d EFA deployment](https://github.com/rajkiranjoshi/llm-d/blob/eks-llmd-p5h100/guides/pd-disaggregation/ms-pd/values_eks_rdma.yaml) uses — privileged mode is not needed.
- **`vpc.amazonaws.com/efa`**: EFA device plugin resource (or DRA `efa.networking.k8s.aws` on K8s 1.34+)
- **hugepages**: Not required for GPUDirect RDMA (GPU memory bypasses host bounce buffers). Only relevant for host-memory transfers.

---

## Comparison with Existing RDMA Checks

| Aspect | IB / RoCEv2 (current) | EFA (new) |
|--------|------------------------|-----------|
| Reachability tool | `ibv_rc_pingpong` / `ping` | `fi_rdm_pingpong -p efa -E` |
| Bandwidth tool | `ib_write_bw --use_cuda` | `fi_rma_bw -p efa -o write -E -D cuda` |
| Address discovery | GID / IP | OOB TCP exchange of raw EFA addresses |
| GPU memory flag | `--use_cuda` | `-D cuda` + `FI_EFA_USE_DEVICE_RDMA=1` |
| K8s resource request | `rdma/ib` or `nvidia.com/roce` | `vpc.amazonaws.com/efa` |
| Container image | perftest (rdma-core) | fabtests (libfabric) + rdma-core |
| Underlying operation | IB WRITE | `fi_writemsg()` over SRD |
| What NIXL uses in production | UCX RDMA READ | libfabric `fi_read` |

---

## TODO

- [ ] Add rdma-core and libfabric as git submodules (or source tarball fetch in Dockerfile)
- [ ] Extend `tools/Dockerfile.dev` with rdma-core + libfabric + fabtests source build
- [ ] Extend `tools/Dockerfile.konflux` similarly
- [ ] Implement output parsers for fabtests format (`fi_rma_bw`, `fi_rdm_pingpong`)
- [ ] Determine EFA topology detection approach (PCIe locality of EFA NICs to GPUs)
- [ ] Add EFA platform config (`pkg/config/platforms/`)
- [ ] Handle EFA DRA (K8s 1.34+) vs device plugin resource naming in node discovery

---

## References

- [libfabric EFA provider docs](https://github.com/ofiwg/libfabric/blob/main/prov/efa/docs/overview.md)
- [fi_efa(7) man page](https://ofiwg.github.io/libfabric/main/man/fi_efa.7.html)
- [fabtests README](https://github.com/ofiwg/libfabric/tree/main/fabtests)
- [GPUDirect RDMA bandwidth on EFA (blog)](https://le.qun.ch/en/blog/2024/12/25/libfabric-efa-3-fi_info/)
- [AWS EFA on EKS docs](https://docs.aws.amazon.com/eks/latest/userguide/node-efa.html)
- [EFA changelog (version history)](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa-changelog.html)
- [aws/libfabric releases](https://github.com/aws/libfabric/releases)
- [llm-d EFA Dockerfile](https://github.com/llm-d/llm-d/blob/main/docker/Dockerfile.cuda)
- [llm-d PR #741 (rdma-core version confirmation)](https://github.com/llm-d/llm-d/pull/741)
- [NIXL with EFA announcement](https://aws.amazon.com/about-aws/whats-new/2026/03/aws-support-nixl-with-efa/)
- [NVIDIA Dynamo EFA setup](https://docs.nvidia.com/dynamo/dev/kubernetes/installation/rdma-setup/efa-on-aws)
