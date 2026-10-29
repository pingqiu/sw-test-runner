# S0a Custody Trial: Independent Review Entry

Ruling 6ae3c15b9c9fd1ba9ca52868 releases the measured 559-line SSH custody
trial for commit on Draft runner#4 and independent review by Claude02.
It does not release TierChaos witness code, lab use, merge or S1 activation.
Ping owns the wider program decision. The final 1,150 cap is unchanged.

## Scope And Charge

| File | Added Go lines |
|---|---:|
| infra/command_pipe.go | 95 |
| owned_ssh.go | 164 |
| custodian_linux.go | 281 |
| custodian_other.go | 12 |
| cli/cli.go | 7 |
| New production charge | 559 |
| Previous conservative charge | 567 |
| Total conservative charge | 1,126 |

These sources are restored unchanged from the captured measured trial. No
line-budget reset is claimed. The ordered binding/envelope cuts remain S0b;
witnessed TierChaos is not implemented and no existing PID-only/fault action
is claimed to meet its missing obligations. Existing queue/lease rules stay.

```text
RunScope -> SSHJobs -> existing infra.Node -> one-shot runner owned-custodian
    enroll -> inert owned-exec child -> authorize pipe -> exec workload
    status / kill / settle -> pidfd + subreaper -> terminal receipt
```

There is no agent daemon or file-based ready/go protocol. The CLI exposes
internal owned-custodian/owned-exec modes, but Engine/YAML does not instantiate
SSHJobs automatically. Deployment is not part of this head.

## Known Gaps To Review First

This is review-ready source, not a claim the draft is correct or merge-ready.
The following are disclosed unfinished safety obligations, not waived limits:

1. The reply decoder currently uses the enrollment work context. Cancellation
   can end the decoder before the cleanup RPC reply; review cleanup custody
   and controller-loss behaviour, including possible late writes and replies.
2. Transport calls need bounded write/session/lock cancellation controls.
   Unknown loss must never become a fabricated settled receipt.
3. The child inherits the opened workload descriptor, but its content hash is
   not verified. ExecutableSHA256 is currently copied from the spec, not proof.
   This is distinct from the deferred deployed-runner/SSH trust binding.
4. Exclusive log-leaf creation does not prove ancestor/symlink ownership.
   Review the actual filesystem custody required by the Scope contract.
5. Linux descendants, escaping sessions, native status, live-at-kill, delayed
   enrollment and EOF/deadline cleanup have no executed Linux controls yet.
   Static implementation plus cross-compilation does not qualify these.

Do not use this head on the lab. Findings go to Claude01 for disposition;
author and reviewer are different. Corrections still have only 24 charged
lines available under the existing cap; do not silently spend beyond it.

## Evidence

Frozen source and command capture:
`V:/share/rdma/codex01-s0a-final-cut-20261029/`.
CAPTURE.json binds the five exact files. Twelve selected local fake/argv
regressions pass; Windows build and Windows/Linux-amd64 test compilation pass.
Linux process execution, SSH and lab tests are NOT_RUN. The prior 61-pass,
eight-mutant Scope evidence remains separate and does not prove this backend.
