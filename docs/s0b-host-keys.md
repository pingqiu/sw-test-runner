# SSH host-key pins (S0b-min, PR1)

Every remote `infra.Node` requires `HostKey`: one operator-approved OpenSSH
public host key (`ssh-ed25519 AAAAâ€¦`, for example). Empty/malformed pins,
multiple lines/keys and authorized-key options refuse. The private client key
remains `KeyFile`. Local/native test execution does not establish SSH identity.

Ordinary `Connect` and each independent `OpenCommand` connection verify the
same explicit key. Reconnects verify it again. There is no TOFU or insecure
fallback. Existing remote scenarios without a pin now refuse before exec.

The CLI maps `topology.nodes.<id>.host_key` to this field; existing environment
substitution can supply it. Supply an independently approved key, not the
result of an unauthenticated key scan. For a new cloud host, obtain and approve
the pin out of band first. Changing an approved pin is an explicit operator
decision; a key mismatch must not trigger automatic replacement.

This patch verifies that the SSH server holds the configured key. It does not
make an inventory/YAML value an authorization, bind an approved endpoint or
artifact set, or construct a trusted RunScope. S0b-min PR2/PR3 must independently
bind those inputs to the operator-controlled deployment policy and authority.
No real-host/provider gate, deployment, lab use or runner cutover is claimed.

CPU controls use real encrypted loopback SSH: approved pin success; missing,
malformed and changed-key refusal before exec; independent command refusal
while an older shared connection remains open; reconnect refusal; and actual
YAML/CLI field propagation. The fixture counts server exec requests so refusal
cannot pass merely because a later command failed.

## Scope and legacy admission refusal

The pin enforcement claim covers `Node.Connect` and independent `OpenCommand`
only. It is not a repository-wide removal of insecure shell SSH. The existing
`scripts/run-block-ci.sh:44` and nested SSH in
`scenarios/internal/rdma-rust-volume-rc.yaml:254,282,349` remain unchanged.
Migration follow-up: pin those transports or retire those paths when their
scenarios move to the approved runner path.

`NewS0bRunScope` refuses scenario/script bytes containing
`StrictHostKeyChecking=no` or standalone/nested `ssh` commands before calling
the authority verifier or creating custody. This is a conservative text gate
(including comments), not a general shell interpreter or proof that arbitrary
scripts are safe. OpenSSH public-key labels such as `ssh-ed25519` remain allowed.
It wraps the existing `NewRunScope`, leaving S0a local/test use unchanged.

PR3's approved worker adapter must use this S0b entrance with the complete,
hash-bound, snapshotted scenario and script bodies it will actually execute;
omitted/unlisted/dynamically substituted inputs cannot qualify. There is no
existing S0b worker activation in this PR and no promise that legacy CLI runs
already construct this scope. A pinned outer connection alone never qualifies
a nested SSH destination. The legacy paths above cannot qualify through this
S0b entrance until separately migrated.
