# Approved generation restart (slice 1)

As-built base: `0be2f7fc218321a2188a29aaddcc64101ff5f737`.
This slice adds an owned process-generation provider. It does not register a
scenario or change the Seaweed smoke workload.

An independently approved policy may name one-shot episodes:

```json
{
  "generation_restarts": {
    "volume-once": {"victim": "volume0", "replacement": "volume1"}
  }
}
```

Both labels must resolve to policy jobs on the same node with the same executable
and hash. Each argv remains independently pinned. The GO must explicitly allow
mutation. A pack retains the handle returned by `RunApprovedPackJob` and calls
`RestartApprovedPackGeneration(ctx, "volume-once", handle)`.

```text
consume exact episode + originating service label
  -> bind old-generation exit intent before signal dispatch
  -> exact-generation KILL witness
  -> owned old-generation exit/descendant settlement
  -> ordinary live-authority replacement enrollment
  -> REPLACEMENT_STARTED receipt
```

The intent is recorded before Kill under the existing owned-job gate, and appears
in the generation receipt. Intent alone never qualifies an exit. Only the
witnessed old generation's SIGKILL exit is expected. An unrelated exit,
lost witness, unsettled custody, revoked authority, foreign handle/label, or
replayed episode cannot earn success. No caller-controlled expected-exit flag is
exposed. Replacement dispatch uses the existing scope's work and hard deadlines;
scope cleanup retains ownership of both generations and possible issuers.

`authority.json` joins generation receipts to job receipts by job ID and approved
label. `REPLACEMENT_STARTED` proves neither service readiness nor KV recovery:
the next slice must join new-generation readiness and the byte-exact workload
oracle before claiming fault recovery.

Reuse choice: retain the runner's custodian, immutable process identity,
kill witness, settlement and authority admission. An arbitrary command/PID fault
API would weaken those joins; embedding the old Python controller would duplicate
ownership. No transport lifecycle or new cleanup framework is introduced.

CPU controls exercise exact policy/consent, ordered settlement, refusal cases,
sticky unrelated exits and a real local custodian restart. They do not qualify
M01/M02, SSH deployment, RDMA exposure, or application recovery.
