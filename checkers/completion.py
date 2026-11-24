"""One repetition verdict for two callers of the same completion policy."""

# Highest priority first; every verdict choice uses this single total order.
COMPLETION_ORDER = ('NOT_RUN', 'FAIL', 'INCONCLUSIVE', 'PASS_NOT_CLEAN', 'PASS')


def rep_complete(phase_receipts, required_phases, check_rows, required_checks,
                 failed_checks, ready, state, host_states):
    """Return verdict, deciding receipts and the existing failed/missing lists.

    Inputs are already parsed by the caller. Phase duplicate handling belongs
    to that parser; this function sees its final mapping without reparsing it.
    """
    if not required_phases:
        raise ValueError("completion policy requires phase obligations")
    candidates = []

    def add(kind, source, name, value):
        candidates.append(dict(kind=kind, source=source, name=name, value=value))

    add('PASS' if ready == '1' else 'NOT_RUN', 'readiness', 'ready', ready)
    failed = sorted(set(failed_checks) | {
        'phase:' + name for name, value in phase_receipts.items()
        if value.startswith('rc=') and value != 'rc=0'})
    missing = [name for name in required_checks if name not in check_rows]
    missing += ['phase:' + name for name in required_phases
                if not phase_receipts.get(name, '').startswith('rc=')]
    for name in sorted(set(failed_checks)):
        add('FAIL', 'check', name, check_rows.get(name))
    for name, value in phase_receipts.items():
        if value.startswith('rc=') and value != 'rc=0':
            add('FAIL', 'phase', name, value)
    for name in required_checks:
        if name not in check_rows:
            add('INCONCLUSIVE', 'check', name, None)
        else:
            add('PASS', 'check', name, check_rows[name])
    for name in required_phases:
        value = phase_receipts.get(name)
        kind = 'PASS' if value is not None and value.startswith('rc=') else 'INCONCLUSIVE'
        add(kind, 'phase', name, value)
    clean = bool(host_states) and all(value == 'CLEAN' for value in host_states.values())
    for name, value in host_states.items():
        add('PASS' if value == 'CLEAN' else 'PASS_NOT_CLEAN', 'host', name, value)
    if not host_states:
        add('PASS_NOT_CLEAN', 'host', 'missing hosts', None)
    rank = {kind: index for index, kind in enumerate(COMPLETION_ORDER)}
    chosen = min(candidates, key=lambda receipt: rank[receipt['kind']])['kind']
    deciding = [receipt for receipt in candidates if receipt['kind'] == chosen]
    verdict = 'NOT_RUN(%s)' % state if chosen == 'NOT_RUN' else chosen
    return dict(verdict=verdict, deciding_receipts=deciding,
                failed=failed, missing=missing, clean=clean)
