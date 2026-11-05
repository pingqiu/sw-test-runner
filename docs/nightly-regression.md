# Nightly local regression (#998), v1

This is an operator-staged nightly job, not a credentialed main poller. No cron
is installed by the repository. Qualify the existing systemd image and one full
eleven-cell sweep on tp01 before enabling it; reserve a suitable test slot.
The script changes no scenario/provider/oracle. No lab hosts or cloud are used.

## Workstation: prepare the night's latest main

Immediately before the nightly slot, use the workstation's existing GitHub
authentication to fetch mono main and record its full SHA. Export that exact
commit with `git archive --prefix=mono/ --format=tar.gz SHA -o mono.tar.gz`.
Export the reviewed runner commit without a prefix to `runner.tar`; include
the two nightly scripts. Supply the already qualified native Python bridge
archive as `native.tar`. Record its source revision separately if older than
the product; requalify on ABI changes. No credentials go into these archives.

Stage these four files in the tp01 job root's `inbox/`, under the same `lock`
used by the worker, publishing only a complete input. `input.json` is:

```json
{
  "mono": "FULL_40_HEX_MONO_SHA",
  "runner": "FULL_40_HEX_REVIEWED_RUNNER_SHA",
  "native_revision": "QUALIFIED_BRIDGE_REVISION",
  "night": "YYYY-MM-DD",
  "image": "sha256:QUALIFIED_EXISTING_LOCAL_IMAGE_ID"
}
```

The date is the UTC night being run. Source discovery/staging is the workstation
operator's responsibility; absent/stale input is an input refusal, not proof
that latest main ran. Use the existing workstation SSH access; do not copy its
key, GitHub token, Huly token or CIFS credentials to tp01. Import the existing
qualified image first; the job never pulls an image.

## tp01: one local result per changed head/night

Choose an owned NVMe root, e.g. `/data/nvme/seats/<seat>/main-regression`.
Set PATH to `/opt/work/go/bin`, the installed Rust toolchain's bin, `/usr/bin`
and `/bin`. The host needs Bash, Python3, jq, GNU timeout/flock/tar and Docker.
With the host cron timezone confirmed UTC, the operator installs the equivalent
of this one entry (otherwise convert 02:00Z to the host's cron timezone):

```cron
0 2 * * * PATH=<toolchain-path> nice -n19 bash /path/to/runner/scripts/nightly-regression.sh <job-root> >> <job-root>/cron.log 2>&1
```

Local flock skips a busy invocation; the same mono SHA or same UTC night skips
without rebuilding. The started SHA/night is recorded before build: interruption
does not authorize a retry. Builds are sequential (`make build`, Go weed, Rust
weed-volume), Cargo jobs=4, one target, all caches in the job root. Bounds are
900s controller, 900s Go, 2400s Rust, 25m CLI/1650s outer per cell, and 30–90s
inspection/collection/disposal operations. Expected wall time is 50–60min;
these finite per-phase bounds also bound the complete job, without an extra
controller. Any failed cell is recorded once and later cells continue.

The eleven cells are EC stage1 Go/Rust, EC2a Go, master_writes, HTTP chaos
volume-restart, C1/C2/C4/C5, and EC reconstruction Go/Rust. Existing schedules
and component limits remain: ordinary S2 and C5 exposure can be NOT_COVERED;
master timing is REPORT_ONLY. Rust2a, DC lab cases and C3/C6 are not included.

`results/<mono-sha>/` holds TABLE.md, raw cell results, input/source/artifact
hashes and SHA256SUMS. EXIT is the terminal job status, written after checksums
and excluded from its own evidence manifest; a missing/nonzero EXIT is not
success even when some component rows passed. Build failure leaves all cells
NOT_RUN with the build failure noted. Container removal is followed by a
successful independent absence query. Only that attempt's source/target/binaries
are removed. Failed disposal preserves work plus `cleanup-required` and stops
new work; hard interruption leaving work also requires operator disposition.
Do not delete that marker or work until the owned-container postflight is clean.

## Workstation: collect, archive and report

Fetch the completed result directory over existing SSH, check EXIT and verify
SHA256SUMS locally. Archive it to `V:/share/rdma/main-regression/<mono-sha>/`;
throttle copies >200MB and compare local hashes with the share's sums rather
than hashing payloads back. Then post TABLE.md on mono #998 with overall job
status, versions, coverage qualifications and archive path. Do not repeat a
table already posted for that SHA. Retain local evidence if collection fails.

For each failing cell, use the exact title `<selector>/<impl>: <failing row>`;
use `go`/`rust` for EC, `mixed` for Go metadata/Rust-volume HTTP cases. Inspect
the raw CLI error to name the first failing row; a job/build failure is also a
finding, not eleven product failures. List **all** open mono issues labelled
`stability`, compare titles for exact equality, and comment on the match or
create one with `stability` + `found:regression`. Provision that label once if
missing. Include SHA, first failure, raw path and classification pending.
No hidden fingerprints, automatic close/reopen, test retries or credentials on
tp01. The operator reconciles an ambiguous GitHub write before trying again.
