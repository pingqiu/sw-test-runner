# VFS / S3 over RDMA

Testing the **RDMA** work — VFS kernel reads and S3-over-RDMA proxy traffic —
from the `seaweedfs_sra` / `sra-next` line.

!!! info "Canonical source"
    The authoritative developer guide is **`DEVELOPER-TESTING-START-HERE.md`** in
    the `sra-next` repo (`C:\work\rdma\sra-next\docs\`), also published on the
    [dashboard](../dashboard.md) `/docs` as *Developer Testing Start Here*. This
    page is the TestOps-side entry point; follow that doc for build/run detail.

## Topology

| Node | Role |
|---|---|
| **M01** | kernel mount / VFS client, RDMA `10.0.0.1` |
| **M02** | volume server + RDMA target, RDMA `10.0.0.3` |

25 Gbps RoCE link between them. The VFS gate runs the kernel side on M01 and the
volume/RDMA side on M02 (ports 9753 / 8103 / 9103 / 7530; RDMA target
`10.0.0.3:7530`).

## Backends

`rdma-loader` reads an object and writes it to a local file using the shared
client code (no proxy):

| Backend | Note |
|---|---|
| `tcp` | baseline |
| `rc` | **native RC — the V1 baseline; best current S3-over-RDMA path** |
| `dc` | native DC; one DC target per source plan; higher p99 than RC on some large objects |
| `ucx` | UCX C-shim; **opt-in** smoke path, **not** faster than native RC today, **not** the default |

```bash
rdma-loader get s3://bucket/key --backend rc \
  --filer HOST:PORT --rdma-map VOLUME_HOST=RDMA_HOST:18566 \
  --output out.bin --sha256
```

## Running via the runner

The VFS read gate is a `build → seed → test → cleanup` scenario
(`sw-rdma-mono-kernel-vfs-rdma-read`) of all-`exec`/`assert_*` phases — kernel on
M01, volume/RDMA on M02. Point it at the shared results root under your project:

```bash
<runner> run <vfs-scenario>.yaml \
  -results-dir /mnt/smb/work/share/testops/results/rdma-qa
```

!!! warning "Don't over-claim"
    Per the canonical doc: UCX is **not** the default backend and **not** faster
    than native RC; `sra-volume` is **not** the upstream SeaweedFS Rust volume.
