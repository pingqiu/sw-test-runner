# Retained r10 DC GET output

r10-dc-load.json is byte-identical captured stdout from job
841dbc6a6b6a1f9c238ad640f67c1364, source runner48f571fc, bundle
20261031-070005-8f34. SHA256
f451c1424c07b05f57a7933a1cd0d15b967808246aa427c803be8d47bcd23b87.
Original result manifest3c241b1085b1d673367ac4f9f2b9f7269b7fffce734754451389c90325982b65
at V:/share/rdma/codex03-read-restart-r10-result-20261031/.

It records402 attempts/399exact/3misses and actual M01 times. Actual ready M is
405658.984701257. NO prospective pre-Kill P was recorded in r10. Its sealed FAIL
and unexecuted named recovery jobs cannot be requalified with a synthetic P.
The current test uses the raw trace only to refuse qualification without P.
The separate actual parent/provider tests prove the new capture placement.