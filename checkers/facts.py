"""One structural ledger judge and shared lease/acknowledgement predicates."""
import re
from datetime import datetime, timezone


RECORD_SCHEMA = {
    'I': {'fields': 7, 'numeric': (1, 2, 5, 6), 'lsn': (4,), 'mono': 5},
    'A': {'fields': 8, 'numeric': (1, 2, 6, 7), 'lsn': (4, 5), 'mono': 6},
    'E': {'fields': 6, 'numeric': (1, 2, 3, 4, 5), 'lsn': (), 'mono': 4},
}


def read_ledger(data, mode):
    """Return records, immutable judged bytes and metrics; reject corrupt input.

    LIVE omits only an incomplete final line. FINAL requires a complete file.
    Intents start at zero, advance by one and resolve before the next intent.
    A terminal matches its intent; block = first_block + seq and monotonic time
    never decreases. One final unresolved intent is valid in either mode.
    """
    if mode not in ('LIVE', 'FINAL'):
        raise ValueError('mode must be LIVE or FINAL')
    if mode == 'LIVE':
        data = data[:data.rfind(b'\n') + 1]
    elif data and not data.endswith(b'\n'):
        raise ValueError('unterminated FINAL ledger')
    recs = {}
    first_block = None
    open_intent = previous_mono = None
    acked = exact = chainbreaks = 0
    previous = last_ack = None
    for n, raw in enumerate(data.decode('utf-8').splitlines(), 1):
        if not raw:
            continue
        p = raw.split('\t')
        k = p[0]
        schema = RECORD_SCHEMA.get(k)
        if schema is None or len(p) != schema['fields']:
            raise ValueError('line %d: malformed record' % n)
        if any(not re.fullmatch(r'[0-9]+', p[i]) for i in schema['numeric']):
            raise ValueError('line %d: malformed record' % n)
        if k != 'E' and not re.fullmatch('[0-9a-f]{64}', p[3]):
            raise ValueError('line %d: malformed digest or LSN' % n)
        if any(p[i] != 'NA' and not re.fullmatch(r'[0-9]+', p[i]) for i in schema['lsn']):
            raise ValueError('line %d: malformed digest or LSN' % n)
        mono = int(p[schema['mono']])
        if previous_mono is not None and mono < previous_mono:
            raise ValueError('line %d: monotonic time decreased' % n)
        previous_mono = mono
        seq, block = int(p[1]), int(p[2])
        if k == 'I':
            if seq in recs:
                raise ValueError('line %d: second intent' % n)
            if seq != len(recs):
                raise ValueError('line %d: expected intent sequence %d' % (n, len(recs)))
            if open_intent is not None:
                raise ValueError('line %d: unresolved intent before next intent' % n)
            if first_block is None:
                first_block = block
            if block != first_block + seq:
                raise ValueError('line %d: expected block %d' % (n, first_block + seq))
            open_intent = seq
            recs[seq] = {'seq': seq, 'block': block, 'sha': p[3], 'kind': 'I', 'iwall': int(p[6])}
            continue
        r = recs.get(seq)
        if r is None or r['kind'] != 'I' or block != r['block'] or (k == 'A' and p[3] != r['sha']):
            raise ValueError('line %d: terminal without its matching intent' % n)
        open_intent = None
        if k == 'E':
            r.update(kind='E', errno=int(p[3]))
            continue
        h0, h1 = (None if x == 'NA' else int(x) for x in p[4:6])
        r.update(kind='A', h0=h0, h1=h1, mono=int(p[6]), wall=int(p[7]))
        acked += 1
        exact += int(h0 is not None and h1 is not None and h1 == h0 + 1)
        chainbreaks += int(previous is not None and (h0 is None or h0 != previous))
        previous, last_ack = h1, r
    return {'records': recs, 'snapshot': data, 'last_ack': last_ack,
            'metrics': {'acked': acked, 'exact': exact,
                        'chainbreaks': chainbreaks}}


def ts(s):
    s = s.strip().rstrip('.').rstrip(',')
    m = re.match('^(\\d{4}-\\d\\d-\\d\\dT\\d\\d:\\d\\d:\\d\\d)(?:\\.(\\d{1,9}))?Z$', s)
    if not m:
        raise ValueError('time %r' % s)
    parsed = datetime.strptime(m.group(1), '%Y-%m-%dT%H:%M:%S')
    base = int(parsed.replace(tzinfo=timezone.utc).timestamp())
    return base * 10 ** 9 + int((m.group(2) or '0').ljust(9, '0'))

def lease_bound(j):
    fence = re.findall('workspace lease: fenced volume=\\S+ replica=\\S+ epoch=\\d+: (.*)$', j, re.M)
    valid = re.findall('workspace lease: renewal failed after .*lease valid until (\\S+)\\): ', j, re.M)
    if not fence:
        return (None, None, 'no fence line: the old owner never fenced')
    # One incarnation per journal: the first fence ends its validity; the last
    # renewal failure supplies the latest bound only for failed-renewal expiry.
    cause = fence[0]
    m = re.search('lease expired at (\\S+) without', cause)
    if not m and not cause.startswith('lease expired after a failed renewal'):
        return (None, cause, 'fence cause is not a lease expiry')
    try:
        U = ts(m.group(1)) if m else ts(valid[-1]) if valid else None
    except ValueError as e:
        return (None, cause, 'unparsable bound: %s' % e)
    if U is None:
        return (None, cause, 'no lease bound logged (cause %r)' % cause[:80])
    return (U, cause, None)

def ack_window(r, U, drain):
    """Classify ACK timing as inside, requested_after_U or completed_after_drain.

    An intent at the lease bound is outside; completion at the drain bound is
    inside. The request-time violation takes precedence when both apply.
    """
    if r['iwall'] >= U:
        return 'requested_after_U'
    if r['wall'] > U + drain:
        return 'completed_after_drain'
    return 'inside'


def resources_absent(leftovers, errors):
    """Decide complete resource observations; unreadable evidence is UNKNOWN.

    Preserve producer diagnostics in order, followed by any observed leftovers.
    The caller collects inventories; this function performs no host actions.
    """
    if errors:
        return {'verdict': 'UNKNOWN', 'details': list(errors) + list(leftovers)}
    if leftovers:
        return {'verdict': 'FAIL', 'details': list(leftovers)}
    return {'verdict': 'PASS', 'details': []}
