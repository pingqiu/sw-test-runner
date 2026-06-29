# Storage TestOps Platform

This repo is the shared scenario runner for SeaweedFS storage work. It should
stay product-facing and practical: run a scenario, collect evidence, publish a
clear pass/fail result.

## Product Areas

| Area | Runner surface | What it proves |
| --- | --- | --- |
| Block | `swblock`, `packs/block`, `packs/v3block` | iSCSI/NVMe correctness, failover, soak, fio perf |
| S3 | `sweeds3`, `packs/s3` | bucket/object API correctness and S3 perf tools |
| VFS | core `exec` plus product scenarios | mount read/write correctness, remount persistence, perf |
| RDMA | `sweedrdma`, `packs/rdma` | M01/M02 hardware gate, RC/DC perf, VFS/object RDMA parity |

## Result Contract

Every serious gate should emit:

- product commit and dirty state;
- exact scenario name and run id;
- correctness witnesses: SHA/MD5 match, remount/read-back, no silent fallback;
- perf rows with stable labels and units;
- logs or artifact path for failed daemon/processes;
- explicit non-claims when a row is unsupported.

## Current RDMA Entry

```bash
sweedrdma validate scenarios/rdma-unified-lab-gate.yaml
sweedrdma run scenarios/rdma-unified-lab-gate.yaml \
  -env mono_ref=rdma/lab-gate-runner-tools \
  -meta project=rdma -meta run_by=$USER
```

The RDMA scenario calls the existing M01/M02 lab runner and records the normal
TestOps bundle/dashboard result. It checks object RC push, RC pull, DC push,
VFS read/write correctness, and loader matrix rows.

## Roadmap

1. Keep block gates as the stability baseline.
2. Add S3 perf scenarios with standard tools such as warp or s3-benchmark.
3. Move VFS read/write smoke and perf gates into product scenarios.
4. Keep RDMA hardware and software-RDMA gates as release blockers for RDMA PRs.
5. Add comparison baselines against external products only when the workload and
   units are identical and reproducible.
