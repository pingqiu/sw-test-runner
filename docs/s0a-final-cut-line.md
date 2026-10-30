# S0a Final 1,150-Line Cut Line

Historical stop record. Ruling 6ae3c15b9c9fd1ba9ca52868 subsequently released
the exact 559-line custody trial for commit and independent review only,
without TierChaos witnesses or lab use. See s0a-custody-review.md. The final
cap remains 1,150; the wider program decision is with Ping.

Status: BLOCKED at the explicit final-cap stop, not S0a delivery or acceptance.
Owner ruling 6ae3bd579c9fd1ba9ca52816 supersedes the earlier 900-line cap.
The existing Scope correction remains production head 298b0e0, with 567
conservatively charged Go additions. This document adds no production code.

## Measured Trial

A non-pushable concrete draft was written and mechanically formatted in the
private worktree, captured, then removed from the integration worktree. The
shared checkout was never edited. The draft was not deployed, run over SSH,
executed on Linux, independently reviewed or enabled in the delivered branch.

| Concrete draft item | Actual additional Go lines |
|---|---:|
| Existing Node one-shot stdin/stdout transport | 95 |
| SSH OwnedJobBackend, protocol and polling | 164 |
| Linux inert exec gate, pidfd and subreaper settlement | 281 |
| Non-Linux fail-closed entrypoint | 12 |
| CLI internal entrypoint dispatch | 7 |
| Total trial additions | 559 |
| Prior conservative charge | 567 |
| Trial projection | 1,126 |
| Remaining under final 1,150 | 24 |

Both owner-ordered cuts are already excluded: deployed-executable/transport
binding and controller envelope integration are S0b. The draft still needs
the retained mandatory TierChaos witnessed apply/effect/restore/absence path,
estimated 150-180 Go lines. This shape therefore projects 1,276-1,306 before
its own safety corrections, at least 126 above the final cap. This is an
implementation-shape estimate, not proof that every smaller design is unsafe.

No pidfd/descendant, Kill, Wait, or fault-restore obligation was silently
weakened to make the number fit. There is no uncounted shell/Python helper.

## Why The Draft Is Not Deliverable

The captured sources are incomplete engineering work, not a reviewed primitive:

- The transport needs bounded cancellation/late-write settlement controls and
  must not lose the cleanup response decoder when work admission is cancelled.
- An installed runner/transport trust binding is only a precondition in the
  draft. A copied workload hash in an enrollment response is not hash proof;
  the pinned executable descriptor still needs actual content verification.
- Remote log creation needs directory/symlink ownership proof beyond exclusive
  leaf creation. Native exit/descendant/error paths need actual Linux controls.
- Witnessed TierChaos and the pack-facing activation are absent. Generic
  ApplyFault callbacks and fake receipts do not substitute for them.

The partial CLI hook and all four draft production files were removed after
their exact source capture. The branch retains only the earlier fake-tested
Scope contracts and this cut-line record. No launcher can select this draft.

## Evidence And Next Owner Decision

Source inventory, hashes, compile captures and draft CLI diff are preserved at
`V:/share/rdma/codex01-s0a-final-cut-20261029/`. Native execution and SSH tests
are NOT_RUN. Local Windows compilation and the scoped fake tests are only
regression checks, not custody qualification.

The earlier 470-570 remainder estimate was low: concrete custody alone consumes
559 in this shape. That estimation error is mine. I stopped rather than spend
the remaining 24 lines on partial fault machinery or request another cap rise.

Claude01 owns disposition: resequence the still-mandatory concrete custody and
witnessed fault slice, or identify a smaller reviewed primitive to reuse. The
final 1,150 cap remains binding, no further implementation is released by this
document. Claude02 remains the independent reviewer; S1/S3 dependent activation
is blocked, and S2/S3/S0b still require the stated whole-program owner review.
