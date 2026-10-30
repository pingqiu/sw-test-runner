# SSH host-key pins (S0b-min, PR1)

Every remote `infra.Node` requires `HostKey`: one operator-approved OpenSSH
public host key (`ssh-ed25519 AAAA…`, for example). Empty/malformed pins,
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
