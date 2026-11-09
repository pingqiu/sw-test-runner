# Maintenance × chaos: implementation checkpoint

This branch implements the controls for the accepted Stage 2 plan, SHA256
`d9109f2e31eddf9a6862e0286c2fcd145d783c93a753c7f0915e088316fe19f0`.
It is **not ready for execution or merge**. `maintenance_task` is forwarded by
the direct CLI but admission deliberately refuses it before artifacts or SSH:
`maintenance runtime is not integrated yet`.

Implemented controls:

- Only the eight approved task/fault pairs, two onprem hosts, replication 001,
  a 1500-second bound and no competing selector are admitted to validation.
- Coverage requires this task's actual VID/shard/generation, operation, bytes,
  owner/boot and conservative fault/GONE brackets. Queued/no-op/foreign work
  remains NOT_COVERED. No cross-host raw monotonic comparison.
- A frozen expected-key/length/hash/delete oracle is independent of task exit
  and refusal. Loss/wrong bytes/resurrection fails; unknown effects or pending
  state cannot be called recovered. Refusals have their own count/verdict.
- Embedded leaf controls execute real Python, including a trickling local HTTP
  server. Every copy announcement starts a new operation, even for the same VID.
  Strict JSON refuses duplicate/aliased decision keys and trailing replies.
- Prototype code invokes the existing product shell/admin/worker paths; it does
  not fabricate `license_valid`, substitute a fake task, or change fault timing.

Remaining before enabling admission:

1. Finish the lifecycle integration, exact-run target backstops and independent
   two-host cleanup within the 300-second reserve, including controller loss.
2. Complete immutable multi-VID balance placement, EC serving-owner/generation
   readback and task/fault evidence joins. A filesystem shard or queued job alone
   is insufficient. Calibrate fixtures without changing the fault schedule.
3. Complete the public-caller modeled-host rehearsal and local real-task cells;
   do not describe the current leaf/control tests as that full rehearsal.
4. Obtain the approved enterprise-license input/binding for the EC worker's real
   detection/execute path. Existing cloud/inventory inputs have no such delivery
   mechanism. Source observation: mono `49daaba1`,
   `enterprise/weed/plugin/worker/enterprise.go:20` and
   `enterprise/weed/admin/dash/plugin_api.go:895`.

No lab token is active. M01/M02 have not been touched by this implementation.
The accepted plan and its review remain the authority; this file records an
incomplete implementation, not a replacement plan or a qualification result.
