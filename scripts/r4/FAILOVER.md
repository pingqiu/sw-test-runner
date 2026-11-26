# R4 fenced failover

Default `failover_mode=automatic` waits for the master's persisted v1 authority
line at epoch 2 after the epoch-1 primary is lost. The line must name a configured
survivor and endpoint, promotion Kind=2, BasisFenced=1 and FencedEpoch=2. Every
operator invocation is counted; automatic mode requires zero. The actual selected
survivor must report primary/healthy/frontend-ready at that epoch. Reconnect the
exact run NQN to its endpoint and run the existing complete before/during-loss
ACK verifier and dropped-write negative control. Metadata alone never passes.

`failover_mode=operator` retains the old held assertion and the authenticated
operator call for a deliberately undecidable automatic-round case. It requires
exactly one operator invocation, then the same fencing proof and full readback.
It is never an implicit fallback after an automatic wait times out. The ordinary
RF3/RF2 survivor topology is expected to decide automatically; operator mode on
that topology fails held, which is the intended guard against mixing the cases.
A separately reviewed undecidable-round fault is needed to count operator mode.

The scenario builds the checker from its runner pin into the checked package.
Evidence retains the authority receipt, operator-call count, selected successor's
status, full intent/ACK ledger, readback result and the existing per-unit journals
before cleanup. New primary selection may be r2 or r3; later connections follow
the persisted endpoint. RF2's no-ACK-before-rejoin and the old node's any-epoch
primary refusal remain unchanged.

Rejoin has two gates: the returning node must never report primary and must
reach replica_ready at epoch 2; then the selected primary's `/status/peers`
must list that old r1 as `State=healthy, Epoch=2`. Its own supporting admission
is not completion of catch-up. No guest writes are sent between those gates.
The existing 240-second catch-up budget decides failure, never eligibility.

The original local rehearsal failed against the former manual-only contract;
it is not relabeled as a pass. The whole sequence must pass again on the selected
RC with primary process SIGKILL before requesting a paid run. Process restart is
local rehearsal evidence, never a cloud power-loss claim.
