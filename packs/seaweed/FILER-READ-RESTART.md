# Filer read-restart: expected outcomes

Approved proposal1abd031d, authority6ae6174b. As-built baseline runner main898e8f73. One filer kill/restart uses the existing generation provider and120s settled-key read-only case. No native changes or host authorization.

- DC errors may be zero: cached descriptors can bypass the filer. If errors occur, each failed GET starts at/after prospective controller P, one contiguous success-bracketed run closes with an exact completion by P+60s. Wrong bytes or unknown completions fail.
- HTTP-before establishes the four original keys. HTTP recovery through the exact replacement filer is the metadata-preservation proof; its named exact GETs occur after replacement readiness and load settlement. Do not credit continued DC reads alone as metadata proof.
- The original filer must be settled before replacement; boot, PID/start, product, config, private persistent directory and socket checks must refuse foreign or still-live state. Readiness binds the replacement filer identity. Master/volume remain unchanged.
- Missing restart role preserves existing volume plan bytes/behavior. Same original work/hard deadlines, cleanup reserve and single-owner S0b lease; no retry/retime. No write-under-fault, resource-flatness or mid-transfer qualification; no Python retirement.

Bounds: +160 production Go, +70 existing-helper lines,6h. Local/fake controls and Draft review precede any separately approved host window. Stop/reprice if socket/metadata proof needs broader lifecycle work.
