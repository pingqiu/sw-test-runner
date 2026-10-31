# Gate 0a Docker inventory extension (schema-only Draft)

This extends the accepted #11 data validator on the same `seaweed.inventory/v1` seam. It implements no Docker command/client, privileged adapter, process custody, deployment, fault provider, lease or cleanup. The shared pack remains unconditionally refused. Schema validity and a matching observation digest are data only, never S0b admission. Runtime Gate 0a is M01 under a separate lab lease, not tp01; a host name in this schema cannot grant that turn.

## Topology and shared metadata

`target: docker` requires exactly one `docker` profile and a distinct one-role container for each member: exactly 3 masters, exactly 2 filers, >=4 volumes, >=4 clients and exactly 1 shared metadata container (14 minimum). The only roles in this KV gate are master/filer/volume/client/metadata. One process/container cannot count as several quorum/client members. A separately named existing physical parent is not part of that count and is never an owned deletion target.

`docker.metadata_store` is `mysql`; `metadata_host_id` names the one metadata member. Each filer has the same required `metadata_host_id`; other hosts omit it. This prevents a declaration of independent filer stores, but does not prove actual filer configuration or shared-database behavior. The runtime adapter/pack must verify it. Existing inventory deadline/service-port declarations remain the ratified deferral; a metadata reference is not an implicit port/default-DSN declaration. Those bindings must land before any runtime activation.

## Placement and generation fields

| Field | Requested data | Resolved data |
| --- | --- | --- |
| `docker.parent` | Existing host ID, canonical management endpoint/public host-key pin, private `run_root` whose final component is run_id, logical executor reference and SHA256. Boot/root stat identities may be empty or explicitly pinned. | Same immutable declarations; actual boot UUID and root device/inode required. No parent key bootstrap from an untrusted container. |
| `docker.network` | Exact run_id + `-net` name; empty or already pinned full network ID. | Full 64-hex network ID. |
| `hosts[].container` | Parent ID; exact run_id + `-` + host ID name; immutable `sha256:<64 hex>` image digest; explicit `privileged:false`; empty or pinned full container ID/netns inode. | Full unique 64-hex container IDs and unique canonical unsigned nonzero netns inode strings. No short IDs/name-only generation proof. |
| Container SSH | Each management address is the parent's address, with a distinct published port (not the parent's SSH port); controller-held logical credential reference. | Canonical independently trusted public host key, same endpoint. Reuse the existing SSH executor; never a docker-exec custody fork. |
| `observation.hosts` | Omitted, as in existing v1. | Exact member coverage and host evidence digests; each boot UUID equals the one physical parent's boot UUID. Full container/netns identities distinguish members sharing that boot. |

The image digest denotes the pinned OCI manifest content, not a mutable tag or a Docker image-config ID. It is not a registry/pull reference. The separately approved artifact/adapter contract must map that manifest to the loaded image/config and prove the mapping; this schema neither pulls nor guesses an image.

Executor/hash/image/boot/network facts are claims to be independently verified by S0b-min and its separately reviewed privileged adapter. This schema neither executes a caller-selected binary nor attests any fact. The future adapter must re-read exact full IDs, image digests, parent boot, namespace and network/endpoint binding before effects. Name lookup, exit0, caller-supplied fields or an inventory digest cannot certify custody or network-fault restoration. This Draft does not define an alternative receipt or authority token.

## Bind storage, not block-device formatting

Docker hosts retain the existing `disks[]` logical ID/mount/explicit format field, but **device_id must be empty and format_allowed must be false** in this profile. These are private directory bind mounts, not exposed block devices. Volume and metadata containers require storage. `container.mounts[]` must cover exactly the disk IDs, without duplicates; its source_path must be the exact `docker.parent.run_root / host.id / disk.id`. Each resolved source has canonical decimal parent `source_device` (stat st_dev) and positive `source_inode`; sources stay on the declared private-root filesystem; directories cannot share one identity or reuse the private root itself.

This is an explicit profile-specific extension to the original targets' `/dev/disk/by-id` contract, not a pretend by-id path inside a container. Original targets retain their exact by-id/format declarations. The future trusted adapter must enforce private-root/ancestor ownership, no symlink escape, actual mount/source identity, container isolation and generation-safe cleanup; this validator performs no stat/resolve/mount/format. Parent paths, devices and the existing host are not granted broad deletion authority. Do not delete by prefix, Docker name or `docker prune`.

`managed_resource_ids` for this target is the exact set of logical ownership keys `docker:<parent_id>:container:<declared_name>` plus `docker:<parent_id>:network:<declared_name>`. These are stable request keys, **not** immutable Docker IDs or live API evidence. The resolved fields carry the distinct full IDs/generations. Join preserves the logical set and names, and preserves every nonempty requested generation pin. Runtime policy must bind the full observed tuple, not operate on these logical names alone.

## Transport is explicit; no software DC claim

- HTTP: `transport:http`, `docker.rdma_mode:none`. Existing per-host RDMA fields are explicitly `not_applicable`. Required unavailable declaration: `[{"transport":"dc","status":"not_applicable","reason":"software_profile_no_dc"}]`.
- Optional rxe RC: `transport:rc`, `docker.rdma_mode:rxe`, unavailable DC reason `rxe_no_dc`. Volume/client hosts declare only supported capability `rc` and existing requested/observed RDMA device/port/GID/MTU/fabric fields; control roles remain RDMA N/A. The future adapter must independently prove the selected device is rxe, not infer a driver from its name or an untrusted profile string. RC needs separate approval; it is not part of HTTP evidence.
- The main inventory envelope applies unchanged: explicit work/hard deadlines, cleanup reserve and per-container service ports are required. HTTP clients and the metadata role declare `services:[]`; an rxe volume additionally declares `rdma` and `rdma_control`. Join preserves these declarations as well as container placement.
- Required `dc`, overlapping supported `dc`, missing/unknown N/A reason or a conflicting mode refuses. There is no HTTP/RC fallback. Data-only RC examples are not hardware or functional evidence.

`docker-sriov` is deliberately **not** added: Gate 0b requires a separate reviewed PF/VF/PCI/device/netns contract and actual CX-5 DC evidence, reusing the existing lab recipe. No PF/VF, cap, driver or fabric state is changed by this work.

## Consumer, tests and remaining gates

[examples/inventory/docker.requested.json](../examples/inventory/docker.requested.json) is a synthetic 14-member HTTP declaration with TEST-NET/benchmark addresses, fabricated public key/digests and no observed facts. It is not a lab inventory or permission to create anything. Observations in CPU controls are synthetic too. After a future trusted capture, exact requested bytes must be preserved for `seaweed-inventory --json join`; only pending observation fields may fill in. Image/parent/executor/placement/storage scope and already pinned generations may not change.

```sh
go test ./inventory ./cmd/seaweed-inventory
go vet ./inventory ./cmd/seaweed-inventory
python3 inventory/schema_contract_test.py
python3 inventory/docker_contract_test.py
```

The CLI retains stable `INVENTORY_INVALID` / `INVENTORY_JOIN_REFUSED` codes and remedy hints, `execution_authorized:false`, and no mutating step/plan surface. Structural JSON Schema checks exact types/keys, topology and transport restrictions; Go additionally checks canonical paths/keys, cross-host references, uniqueness, scope and exact Join pins. The real compiled CLI is exercised by both explicit main-guarded Python contract tests; importing them triggers no build or remote work. Duplicate reflection traversal was folded into one shape walk while touching that code; original case/null regressions remain required.

Before any Docker runtime use: accepted schema + reviewed S0b-min PR3 and privileged container adapter contract + deadline/ports bindings + shared pack implementation + authentic host/lease/deadline GO + whole-flow off-lab rehearsal. Existing witnessed kill/pause/network fault providers, latest-ACK oracle, generation collectors and exact cleanup/absence must be reused. No provider is implemented/activated here and no Docker/host/lab/cloud call has run.
