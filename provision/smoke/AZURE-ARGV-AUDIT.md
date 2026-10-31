# Azure argv audit: plain JSON lists

Scope: every explicit Azure invocation in the cloud run/verify/destroy/cleanup controller, `provision/smoke/smoke.go`. `cleanup --run` selects the same destroy implementation. No wrapper edits, Azure calls or credentials in these controls.

| Call site | Complete argv shape | Dynamic values and cmd.exe audit |
|---|---|---|
| RG absence/pre-create | `resource list --subscription SUB --resource-group RG --output json --only-show-errors` | SUB is validated UUID; RG is `[A-Za-z0-9_-]{1,90}`. No query, shell expression or quote. All returned resources count, including free resources. |
| Subscription residue | `resource list --subscription SUB --output json --only-show-errors` | Same SUB. Go selects this run's tag OR any VM/disk/publicIP; unrelated free resources ignored, never deleted. |
| Explicit OS disk tags | `tag update --resource-id ID --operation Merge --tags seaweed_run=RUN expires_at=UTC --only-show-errors` | ID is the exact Terraform exported OS disk ID, checked against the expected subscription/RG/disk prefix; the pinned module generates its disk name. RUN is the safe validated output basename; UTC is RFC3339. `/`, `:`, `=`, `-`, `_` are ordinary cmd characters. No query or shell expression. |

Search: these are the only three `call(...,"az",...)` sites. Literal argv contain none of `| & < > ^ %` or quote characters. No executable path/credentials are placed in their arguments. Terraform's provider/auth internals are not hand-written az commands in this controller.

The existing Windows wrapper is unchanged (SHA256 e5291aa8891aa06beee9eb0b868e48bf11d6b9a4b42f6b2db5519597feebae3f). The explicit test_windows_az_wrapper.py control copies those exact bytes and replaces only the az.cmd executable pathname with a fake local batch file. It invokes the real WSL /init -> Windows cmd.exe -> batch -> Windows Python argv path. It performs no Azure operation and has no import-time side effects. It requires local WSL interop and C:/Python3/python.exe; run explicitly with --argv-file, --wrapper and a fresh --scratch under the seat.

Evidence: old compound query RED (argv not preserved); 144 emitted lifecycle argv / 12 distinct argv GREEN. The Go controls emit real composed run/verify/destroy call arguments, including ten disk tag updates. Resource-object controls retain all RG entries; select tagged NIC/NSG and foreign VM/disk/IP; ignore unrelated free resources and wrong tags; null, old string arrays and missing id/type fail. Existing timeout, UNKNOWN and killed-controller recovery remain non-clean until proven settled and empty.

Raw output schema changes from CLI query-produced string arrays to actual ARM resource objects; test fakes are updated accordingly. No observer or workload gate changed. Cumulative production additions: 563 versus7ae22dde, 3 above prior560; reported to Claude01. No new cloud window until review/merge/re-pin/token.