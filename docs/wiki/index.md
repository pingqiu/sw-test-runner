# sw-test-runner — TestOps Wiki

A cross-product **TestOps platform** for SeaweedFS: one YAML-driven scenario
runner that tests **block (iSCSI/NVMe)**, **S3**, and **VFS / S3-over-RDMA** on
the shared m01/m02 lab, with a global read-only dashboard so every run is
observable.

If you were handed this link to start testing, read in this order:

Related local docs:

- [Seaweed Block Engineering Wiki](http://192.168.1.135:8010/wiki/) -
  Kubernetes block-storage, CSI, operation-layer, lifecycle, and
  storage-failure design.
- [Seaweed RDMA Engineering Wiki](http://192.168.1.135:8011/wiki/) - Rust
  RDMA data path, native RC/DC, UCX, pull-RDMA, and future GPU-style
  destinations.

1. **[Handbook](../testops-handbook.md)** — lab/env access, how to run, watch a
   run, the build→seed→test→cleanup process, gotchas.
2. **[Cross-Product TestOps Standard](../cross-product-testops-standard.md)** —
   the contract every scenario follows so runs are observable and projects don't
   collide on the shared lab.
3. **[Scenario Spec](../scenario-spec.md)** — the exact YAML schema.
4. **[Tutorial](../tutorial.md)** — hands-on walkthrough (optional).

## The 5-minute model

Every scenario is self-contained and runs four phases:

```text
build  →  seed  →  test  →  cleanup
(get the bits) (data/daemons) (assert) (zero residue, always)
```

A run produces a **bundle** (`manifest.json`, `status.json`, `result.html`,
`result.xml`, `artifacts/`). Point the runner at the shared results root and the
run shows up on the [live dashboard](dashboard.md).

```bash
# block / kv / v3 block
swblock run scenarios/public/e2e-block.yaml -env <env.yaml> \
  -results-dir /mnt/smb/work/share/testops/results/block-qa
# S3
sweeds3 run scenarios/s3-smoke-chain.yaml \
  -results-dir /mnt/smb/work/share/testops/results/s3-qa
```

## Where things are

| Section | What |
|---|---|
| [How It Works](how-it-works.md) | The design — core + packs + binaries, the run pipeline, phases, variables, status. |
| [Run Lifecycle & Cleanup](lifecycle.md) | build → seed → test → cleanup, cluster control, the cleanup discipline. |
| [Scenario Syntax](syntax.md) | The scenario YAML — fields, phases, actions, variables. |
| [Code Map](code-map.md) | Repo layout — where the engine, packs, actions, and binaries live. |
| [Packs & Binaries](packs-and-binaries.md) | The per-product binaries and which action packs each one carries. |
| [Scenarios](scenario-catalog.md) | The scenario catalog (public set) + how to list / validate / run. |
| [Deploy & Operate](deploy.md) | Build, lab topology, nodes, the dashboard service, disk janitor. |
| [Product Testing Guides](guides/block.md) | Per-product recipes: block, S3, VFS/RDMA. |
| [Live Dashboard](dashboard.md) | The global run + docs view at `http://192.168.1.181:9099/`. |

## Serve this wiki

```bash
pip install -r requirements-docs.txt
mkdocs serve            # http://127.0.0.1:8000
# or: make wiki
```

The wiki is plain Markdown under `docs/`; the live, always-on view of *runs*
(not docs) is the [dashboard](dashboard.md).
