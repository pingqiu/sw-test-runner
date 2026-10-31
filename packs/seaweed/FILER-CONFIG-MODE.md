# Filer replacement config mode

R12 at runner `52d61509` failed before replacement product exec: initial
`filer.toml` was 0664, while replacement correctly rejected group-write.
Creation now requests 0600 with exclusive create. The rejection stays unchanged.

The local control runs the actual profile CLI for deploy, SIGKILL/join and
replacement under umask 0002. An ELF fixture supplies metadata and a Unix socket;
it substitutes for the product only, and proves no KV or hardware behavior.
It was also executed as UID/GID 1001:1001, matching the worker. The unchanged
parent fails; the successor passes. Deliberately changing config to 0660 still
refuses before replacement publication.

## Creation audit

Scope: `packs/seaweed/worker/lab_profile.py` at `52d61509`, searched for
`mkdir`, `open`, `write_`, `socket` and `marker`, then followed `launch`,
`prepare_filer_replacement` and `private_root`. This audit concerns this helper's
replacement path, not every product-created LevelDB file.

|File or directory|Creation and replacement requirement|Umask 0002|
|---|---|---|
|Output root|Created by admitted runner; helper requires owned private directory|Existing 0700 required|
|`filer/`, `filer/ldb2`|Explicit mkdir 0700; ownership/mode plus retained device/inode checked|0700|
|`home/`, `cache/`, `tmp/`|Explicit mkdir 0700; private root contains them|0700|
|`filer.toml`|Previously open(x), now explicit exclusive 0600; replacement requires canonical stable bytes and no group/other write|0600 after fix|
|`filer-generation.json`, `filer-replacement-generation.json`|Exclusive open(x), umask-dependent; contained in private 0700 root. Replacement reads original identity; it does not require marker mode0600|0664; no creation/guard disagreement|
|`filer.sock`|Created by product in the private root, not helper. Helper requires exact owned inactive socket before unlink; it does not require socket mode0700|Product/umask-dependent, private directory prevents external access|
|`master.toml`|Exclusive open(x); no filer replacement check consumes it|0664; outside this replacement path|
|Service stdout/stderr and run marker|Created by runner custody/output machinery, not this helper|No new helper creation policy|

Metadata directory identity and group-write refusal are unchanged. No host file
was repaired, and r12 remains a sealed FAIL. A new reviewed packet and launch
window are required for any further lab attempt.
