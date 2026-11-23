# Shared checker leaves

`facts.py` owns `read_ledger`, `lease_bound`, `ack_window` and `resources_absent`. Packet adapters
call these entries; they do not retain a second structural or timing predicate.
The library uses only Python's standard library. Pure fact functions perform
no host actions; the separate authenticated absence CLI observes local
inventories read-only and performs no cleanup, SSH or ownership operation.

`read_ledger(bytes, mode)` returns records keyed by sequence, the exact judged
snapshot bytes, the last ACK and a metrics dictionary with `acked`, `exact`,
`chainbreaks`. LIVE ignores only an incomplete final line; FINAL requires
termination. Complete malformed rows, duplicate blocks and unmatched terminal
records raise `ValueError`. Intents start at zero and advance by one; each resolves before the next
intent. Blocks advance from the first block as `first_block + seq`, rejecting
gaps, reversals and duplicate blocks. Monotonic timestamps never decrease. One final unresolved intent
remains valid evidence in either mode.
Metrics are not a PASS verdict. Classification and image verification stay in
the calling packet in this extraction; they consume the shared records.

`block = first_block + seq` is a property of the current sequential load
generator, not of every well-formed ledger. A load that wraps or restarts at
another base must declare that behavior as sealed policy. This reader currently
supports only the sequential policy above; a different policy needs a reviewed
implementation. Any change to an accepted fact definition requires
fact-semantics review before merge.

`lease_bound(journal)` preserves the logged lease-bound interpretation and
returns `(bound_ns, cause, error)`. Missing evidence returns an error, never an
invented bound. The last renewal-failure bound is a fallback only for a
failed-renewal expiry fence; revocation and other causes return an error. `ack_window(record, bound_ns, drain_ns)` returns `inside` only for
an intent strictly before the bound and completion no later than bound + drain;
otherwise it returns `requested_after_U` or `completed_after_drain`. Callers must not treat missing
lease evidence or guest-kernel success as a target durability proof.

`resources_absent(leftovers, errors)` returns a named `verdict` and `details`:
UNKNOWN if any inventory failed, otherwise FAIL for observed leftovers, else
PASS. Producer errors precede leftover details, preserving their order. Its
inputs must be the complete observations from the authenticated collector.

`resources_absent_cli(packet_file, arguments)` runs the shared read-only local
collector in `resource_inventory.py`, using the caller's run.env as data and
its packet location for the existing scope rules. It checks systemd units,
listening TCP ports, mounts, NVMe, loops, declared devices, processes and owned
roots. Each producer's own status is checked; failed reads stay UNKNOWN.
It preserves the absence CLI's exit codes and output. The loader binds the
single pure deciding function; packet adapters cannot supply another one.
The collector supports the existing synthetic inventory-root/systemctl seams;
loading the library alone performs no observation.

`loader.load_library(root, manifest_sha256)` requires the exact manifest pin,
checks every declared code file, then loads the verified facts and inventory
modules. The closure includes facts.py, loader.py, resource_inventory.py ops_rmguard.py and unit_properties.py. There
is no ambient import fallback. The loader itself must be authenticated by the
calling packet before it is executed; its hash is also part of `library.json`.
A packet's own seal must cover its bootstrap, configuration and library digest.
`library.sha256` is the repository's current manifest digest. Any code update
requires new file hashes, a new manifest digest and a new packet seal. Old
packets and evidence keep their previous versions.

Run `python -m unittest discover -s checkers -p 'test_*.py'`. Synthetic controls
exercise duplicate-block and orphan-ACK rejection, FINAL/LIVE tails, ACK
boundaries, missing lease evidence and modified/missing pins. Eight isolated
source mutants demonstrate the parser ordering and lease/ACK controls
are sensitive to their deciding conditions. No packet contents, captured
artifacts, host addresses or credentials belong in this directory.

Absence controls cover complete empty inventories, leftovers, failed producers,
unreadable inventories, self-unit exclusion and usage. A targeted source mutant
ignores inventory errors and falsely returns PASS; the ordinary control returns
UNKNOWN. Loader controls reject every missing/modified closure member and a
stale published manifest pin.

Operational removal is separate from facts: `loader.load_rmguard(root, pin)`
verifies the same complete manifest and returns `run(packet_file, arguments)`
from `ops_rmguard.py`. Loading performs no deletion. `facts.py` never imports
this module. The packet location selects its existing run.env bindings;
canonical roots, directory-fd ancestry and identity, complete mount inventory,
and final identity checks retain their existing refusal and exit protocols.
The remover deletes the contents and the root itself only after all admission
checks pass. A mount at the root or beneath it refuses without deleting either.

Ownership-ledger and lock-holder helpers remain identical per packet. Their
stdin-before-staging and controller event-parser consumers must be addressed
with the launcher rework before those helpers can be shared.

Removal controls require POSIX directory-fd operations. Skipping them on another
platform is not qualification evidence; run them on a supported POSIX host.

## Fact boundaries

- Guest acknowledgement window reads a guest write record and lease/drain bounds;
  proves its timing classification; does not prove a revoke event or detached authority.
- Revoke-event completion reads the stop's revoke event and its outcome/timestamps;
  proves that event completed as recorded; does not prove the detached generation or guest acknowledgement window.
- Detached-authority proof reads the detach/revoke proof, authority and probe;
  proves the expected generation/epoch and probe evidence match; does not prove revoke-event timing or guest write durability.
- Unit inactivity reads current unit properties with checked producer status;
  proves the unit is not active now; does not prove the unit succeeded.
- Invocation outcome reads properties and manager records bound to a recorded invocation;
  proves this invocation ended with this result; does not prove the unit is gone.

Only an identical unit-property observation is shared here. Each packet retains
its inactivity/completion verdict and expected-exit policy. An expected exit in
one policy does not authorize accepting a failed unit in another packet.

`show_unit_properties(unit, timeout, systemctl)` reads one property set within
its caller's remaining producer budget. A failed process, timeout, missing
required property or empty load/active state raises `ValueError`; packet
adapters translate this to their existing UNKNOWN path. It returns properties,
never a PASS or completion decision. The existing reader represents an absent
ExecMainStatus as `-`, including for services; that marker is not a successful
exit status. Loading the pinned module does not invoke systemctl.
