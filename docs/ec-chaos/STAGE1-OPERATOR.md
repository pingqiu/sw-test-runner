# EC stage 1 operator entry

The `seaweed_ec_stage1` approved-run action and its S0b wrapper are retired.
Use `seaweed-cloud direct` with `ec_stage1:true` on a disposable local SSH/systemd
container, represented as target=onprem. See
[EC-STAGE1.md](../../packs/seaweed/EC-STAGE1.md) and the runner README.

Historical approved-run receipts remain evidence of their pinned source heads;
they do not authorize new runs. S1 can PASS; S2 remains NOT_COVERED without a
product reconstruction marker, even when all mandatory byte/deadline rows pass.
