# One-VM smoke lifecycle (supersedes the outer-S0b proposal)

Ruling 6ae4d9fb: dedicated test RG, one lease/run at a time, one Linux command.
No outer S0b run, second admission or delete-identity fence.

`cmd/seaweed-cloud` uses the pinned Azure module/lock, saves a Terraform plan,
applies once, exports inventory, calls the existing #19 shared pack through its
normal RunApprovedRequest, then destroys. Azure CLI lists the whole RG
independently of Terraform state. Only successful destroy plus an empty list
can yield COMPLETE; workload failures remain failed after clean teardown.
Partial/UNKNOWN apply also enters teardown, without retry. Remaining IDs and a
nonzero JSON result direct manual recovery.

The module tags resources with fixed expiry. There is no native shutdown
schedule or DevTestLab requirement. Entry bounds the deadline to 60 minutes and
estimate/budget to $3. The run destroys and lists the full RG; after session
death the next session runs the same destroy/verify before any new create.
Resources may continue billing until cleanup; no automatic cutoff is claimed.

Credentials stay on Linux. Guests receive only the existing SSH public key.
No Automation, MI, custom role, copied tokens, new lease implementation or
self-signed GO. Normal S0b observations, protected artifact installation and
claimed request/GO remain prerequisites; Terraform output cannot mint them.

See [the recipe](../smoke/README.md). Code/tests are local/fake only; no account
API, cloud plan/apply, lab or spend is qualified or authorized by them. Runtime
still needs manager's launch token.
