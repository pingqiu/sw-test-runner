# Seaweed approved execution entry retired

Baseline: runner main `0d4c71c7`. This removes Seaweed's execution adapter,
not the generic approved/RunScope/custodian mechanisms in the runner root.
The existing host installs and historical packets are unchanged.

## Entry points

- `sw-test-runner approved-run` is now an unknown command. Its former CLI
  implementation hard-coded `seaweed.RegisterPack`; no replacement alias is added.
- `RegisterPack` keeps only read-only `seaweed_inventory_validate` and
  `seaweed_plan`. Mutating phases, read-restart, soak and lease-envelope
  registration are absent even when the caller allows mutations.
- `seaweed-pack plan/profile`, ProfileInput/BuildProfile and typed plan APIs
  remain local data producers. `PLAN_DATA_ONLY` replaces `S0B_REQUIRED`;
  inventory-only planning returns `PLAN_INPUT_REQUIRED`. Neither grants effects.
- Use `seaweed-cloud direct` for existing hosts; cloud lifecycle remains on
  the cloud entry. Cluster chaos migration is separate, after this deletion merges.

## Retained shared code

Plan/Step decoding, containsStep, inventory/profile/job binding, strict verdict
and clock leaves, soak runtime scheduling, payload/oracle/capture helpers and
read/window/freeze assessors remain. Only approved execution methods and their
session/runtime adapter are removed. `readBound` and `pollBound` move unchanged
from soak_adapter_linux.go to soak_reader_linux.go; reader controls now instantiate
soakWindow directly. The successful/failed real assessor-argv controls remain.

Cloud HTTP input fixtures now read synthetic inventory data directly, without
constructing a CPU executor, GO, policy bootstrap or lease. Strict soak-binding
negative cases now call the retained bind function with declared ProfileBundle
inputs. Their expected refusal predicates remain. The write clock-rounding and
default-restart-role byte regressions remain as pure tests.

## Required checks

Old-head RED/new-head GREEN: retired mutating action lookup and retired CLI
unknown-command controls. All data-only/profile/reader/binding/verdict/clock
negatives plus direct selectors/lifecycle tests must pass. Full Go module tests,
relevant race and vet cover remaining callers; no new live qualification is claimed.

## Deferred cluster contract

Placement B is accepted for stage 1 and C1/C2/C4/C5, process faults over HTTP,
first on an existing systemd-capable local container, one case per run. Preserve
observed two-copy and owner evidence from the old cluster baseline, not merely
replication=001. C5 unknown copies never earn excusal; co-location is not host
failure tolerance. C3/C6 are explicit NOT_RUN on direct; existing cloud paths stay.
Network namespaces/platform extensions are not approved. Report before exceeding
350 new production lines for the later process slice.
