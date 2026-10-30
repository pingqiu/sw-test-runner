# Seaweed inventory v1

This is the data seam between cloud provisioning and one shared deployment pack.
It does not start jobs, resolve credentials, format disks, contact hosts, grant GO,
or replace the runner admission/lease/cleanup contract.

```sh
go run ./cmd/seaweed-inventory validate examples/inventory/http.requested.json
go run ./cmd/seaweed-inventory join examples/inventory/http.requested.json examples/inventory/http.resolved.json
```

Exit0 means `valid_data_only` and always reports `execution_authorized:false`.
Exit2 refuses malformed input, an unknown operation or an invalid join. Only
bounded regular files are read. No deploy/run operation exists.

## Request, observe, authorize

1. Terraform emits **requested** inventory; on-prem users write the same format.
   Keep the exact bytes: resolved observations bind their SHA256, not a rewritten
   or normalized JSON equivalent. Repository JSON fixtures use LF on every
   platform so a Windows checkout does not invalidate their bindings. Empty host-key/NIC/device hints are explicit
   pending facts, not a permission to discover and trust anything.
2. **Resolved** inventory records per-host boot ID, actual capture time and
   evidence digest, exact host-key pins, NIC/MTU and by-id disks. `Join` preserves
   the request host/resource sets, endpoints, roles, credential references, roots,
   mounts, format declarations and every supplied RDMA/device hint. Observation
   can fill pending facts but cannot expand scope. Evidence hashes are data,
   not signatures or proof that the observation is authentic.
3. The separately reviewed S0b-min adapter must verify out-of-band SSH pins,
   deployed runner/interpreter/config/artifact bytes, authoritative GO, policy,
   allowed hosts/roots/disks/transport, live lease and original deadlines before
   it may authorize jobs. A JSON flag, valid digest or this CLI cannot do that.

Every host has stable `id`; cloud `resource_id` must occur in the managed list,
while on-prem uses an empty resource ID/list. Distinct cloud hosts cannot claim
one resource ID. One host may have several roles. Inventory validates the
roles it declares; scenario-specific role counts are the shared pack's job.
Credential references are logical names, never private-key paths or tokens.
Real cloud credentials, SSH private keys, Terraform state and raw outputs stay
private on the Linux controller, never on tp01 or guests. Public host-key pins
must be canonical `algorithm base64` keys without options/comments.

## Transport and disks

Transport is required and accepts only `http`, `rc`, `dc`; unknown values never
select HTTP. HTTP and control-only hosts explicitly use RDMA `not_applicable`.
For RC/DC, client/volume roles require requested capability, then observed device,
port, GID index (including valid index0),32 lowercase hex GID, link layer, active
RDMA MTU and fabric-evidence SHA256. Data-NIC MTU is separate from active RDMA MTU.
The execution preflight still has to prove actual RC/DC capability and PFC/ECN
or IB state from trusted evidence. EFA or a generic cloud RDMA label is not a
substitute. No transport fallback is implemented.

Disks have stable logical IDs, exact observed `/dev/disk/by-id/...`, canonical
mounts and an explicit `format_allowed` boolean, including false. No wildcard
or traversal is allowed. Format permission remains subject to independent GO
and policy. Lexical validation cannot prove that two distinct symlinks are not
the same block device; the privileged preflight must join actual device identity
and ownership before formatting. It must not format the OS disk or a shared disk.

`schema.json` is JSON Schema2020-12, usable without Go. `Decode` is authoritative
for byte limits (1MiB), strict UTF-8, duplicate/unknown fields, canonical paths,
IP/key syntax and identity uniqueness; `Join` adds cross-document binding.
Schema validators alone cannot express every semantic or cross-file check.
Unknown v1 fields are refused; extend the versioned contract through review.
The Go library and CLI have no Python runtime dependency.

```sh
go test ./inventory ./cmd/seaweed-inventory
python inventory/schema_contract_test.py # explicit CPU check; needs jsonschema
```

The examples use TEST-NET addresses, synthetic public keys and fabricated
observations solely as contract fixtures. They are not an M01/M02 inventory,
trusted host receipts, deployment admission or a hardware PASS.

### Agent output contract

JSON is the default; `seaweed-inventory --json validate FILE` (or `join REQUESTED RESOLVED`, `schema`) makes the choice explicit. Validate/join emit one `seaweed.inventory-result/v1` envelope on stdout for success and refusal. Stable fields include `status`, `code`, `remedy`, `execution_authorized:false`; success adds state and exact input digest. `schema` emits the structural JSON Schema itself. Exit 0 means valid data only, 2 means refusal, 1 means the output could not be written. Refusals do not echo raw input or local paths.

| Code | Remedy |
| --- | --- |
| `OK` | Obtain reviewed S0b admission before effects; validation grants none. |
| `USAGE` | Choose validate, join or schema; no run/apply command exists. |
| `INPUT_UNREADABLE` | Supply a readable bounded regular local file. |
| `INVENTORY_INVALID` | Correct the input against the emitted v1 schema. |
| `INVENTORY_JOIN_REFUSED` | Validate both documents, preserve scope and bind the exact request digest. |

This CLI has no mutating steps. Executable plans belong to the reviewed shared pack, not to inventory validation.

### Execution envelope and producer/consumer binding

The former R3 deferral is closed. `deadlines` is required: canonical UTC
RFC3339Nano `work` and `hard`, plus positive `cleanup_reserve_ms` <=60000.
`work + reserve <= hard`. Validation does not check the current clock: saved
historical evidence remains decodable; live S0b admission enforces freshness.
Every host declares `services` as exact name/port pairs. Master/filer/volume
HTTP and gRPC listeners, S3 HTTP, RDMA push data plus RC control (and DC
control for DC) listeners are mandatory for
the corresponding role/transport. Names and ports are unique on a host; there
is no implicit +10000 arithmetic or transport fallback. Empty client services
are explicit. Optional `kv_coordinator` declares that listener when installed.
Join preserves the complete service set and original deadlines exactly.

`Resolve(requested, observed)` is the data producer: it binds the exact request
bytes and refuses altered declarations before emitting resolved JSON. It does
not discover facts or create trusted receipts. `Bind(requested, resolved,
ConsumerBinding)` is the consumer seam: it pins both byte streams and joins the
run/transport/artifact/deadline envelope and exact host/SSH/root/boot set to
values from independent admission. A caller-created ConsumerBinding is not GO.
The shared pack supplies those values from S0b's admitted request/readbacks;
raw variables and successful validation cannot authorize effects.

Cloud module outputs predating this successor lack required fields and now
refuse Decode. Their static acceptance is not runtime compatibility: producers
must emit the same explicit deadlines/services (never infer them) before use.
The independent provisioning cost/deadline declaration is not substituted for
this inventory envelope. No provider, host or cloud qualification is claimed.

Decoding uses exact JSON tag spellings recursively, before Go struct decoding; unknown, noncanonical and duplicate-by-case keys are refused. Optional primitive fields may be omitted, but supplied null is rejected where the published schema requires a string/integer. Published nullable observation/GID-index/N/A-capabilities declarations remain subject to their existing state-specific validation.

Gate 0a data extension: [Docker schema-only profile](DOCKER.md). It adds container placement/generation/storage and explicit software DC N/A declarations; it does not qualify a Docker/VF adapter or authorize execution. Gate 0b remains unsupported.
