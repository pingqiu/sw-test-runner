# Prepared option A; not selected or published

The active `r4-node-loss.yaml` and `r4.sh held` remain unchanged. No paid
attempt or local resource run is authorized by this branch.

If the owner selects automatic fenced failover as the release contract:

1. Keep the existing writer through the primary loss and stop it with its
   intent/ACK ledger intact. Save the pre-fault acknowledged LSN barrier.
2. Replace the held window with a bounded wait for the authoritative line at
   e+1. Read its actual successor, not a hardcoded r2; collect that replica's
   fresh status and reconnect the exact run NQN to its endpoint.
3. Before any operator call, run `auto-p2.py` with that authority receipt,
   status, pre-fault epoch/holder/barrier, zero operator calls, device, ledger
   and the existing `r4.sh`. It checks the promotion/fencing-round evidence
   and serving identity, then runs the existing full ACK readback. A missing
   fencing proof or lost ACK fails; new-epoch readiness alone is insufficient.
4. Do not execute the old operator-failover step against r1@1 after automatic
   promotion. Continue rejoin and RF2 no-ACK checks using the observed new
   identity and epoch. Keep the existing any-epoch old-primary refusal.
5. An operator-only fallback needs a separate controlled undecidable-round
   case; do not silently fall back to operator failover when this P2 fails.

This branch prepares the checker and its no-resource controls. Wiring the
alternative into the whole sequence and rerunning RF3 then RF2 remain gated
on the owner's choice. The prior failed rehearsal is not relabeled as PASS.
