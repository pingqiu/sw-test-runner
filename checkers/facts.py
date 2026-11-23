"""One structural ledger judge and shared lease/acknowledgement predicates."""
import hashlib
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


def classify_write(record, boundary):
    """Classify one validated record by LSN, never its wall-clock timestamp.

    Exact ACKs require H1 = H0 + 1. Missing or inexact LSNs stay uncountable;
    failed or unresolved writes permit either their exact content or zero.
    """
    if record['kind'] != 'A':
        return 'either'
    h0, h1 = record['h0'], record['h1']
    if h0 is None or h1 is None or h1 != h0 + 1:
        return 'lsnless'
    if h1 <= boundary:
        return 'must'
    if h0 >= boundary:
        return 'lost'
    return 'straddle'


def classification_counts(recs, cls):
    c = {k: 0 for k in ('must', 'lost', 'straddle', 'lsnless', 'failed', 'inflight')}
    for s, k in cls.items():
        if k == 'either':
            c['failed' if recs[s]['kind'] == 'E' else 'inflight'] += 1
        else:
            c[k] += 1
    return c


def _raced_seq(recs):
    """Return the last ACK only when H0 is known and H1 is unknown."""
    acked = [r for r in recs.values() if r['kind'] == 'A']
    if not acked:
        return None
    r = max(acked, key=lambda x: x['seq'])
    return r['seq'] if r['h0'] is not None and r['h1'] is None else None


def image_exact(records, image, boundary, blocks, block_size=4096, raced='none'):
    """Judge a supplied binary image stream against validated ledger records.

    The stream starts at byte zero; no path is opened and no host is contacted.
    Exact ACKs must be present at/below L and absent above L. Uncertain writes
    allow zero or exact bytes, but unresolved ACK LSNs cannot yield PASS.
    A raced-write exception requires the caller's explicit final-row decision.
    The result names PASS, FAIL or NOT_COUNTED with counts and diagnostics;
    stream/decoding failures propagate instead of producing a verdict.
    """
    if blocks <= 0 or block_size <= 0 or boundary < 0:
        raise ValueError('positive image geometry and nonnegative LSN required')
    cls = {seq: classify_write(record, boundary) for seq, record in records.items()}
    bad = []
    rs = _raced_seq(records)
    want = raced
    if want != 'none':
        if rs is None or want != str(rs):
            actual = 'none' if rs is None else str(rs)
            bad.append("raced decision %s is not this ledger's raced write (%s)" % (want, actual))
            rs = None
    else:
        # The record's shape alone never supplies the final-row decision.
        rs = None
    if rs is not None:
        cls[rs] = 'must' if records[rs]['h0'] + 1 <= boundary else 'lost'
    outside = sorted(record['block'] for record in records.values() if not 0 <= record['block'] < blocks)
    if outside:
        bad.append('%d ledger record(s) outside the %d-block geometry (first block %d)' % (
            len(outside), blocks, outside[0]))
    c = classification_counts(records, cls)
    raced_messages = []
    byblock = {record['block']: record for record in records.values()}
    zero = bytes(block_size)
    ack_lsns = [record['h1'] for record in records.values() if record['kind'] == 'A' and record['h1'] is not None]
    hmax = max(ack_lsns, default=0)
    f = image
    for block in range(blocks):
        data = f.read(block_size)
        if len(data) != block_size:
            bad.append('short read at block %d' % block)
            break
        record = byblock.get(block)
        if record is None:
            if data != zero:
                bad.append('block %d: never written but not zero' % block)
            continue
        category, digest = cls[record['seq']], hashlib.sha256(data).hexdigest()
        if record['seq'] == rs:
            ok = (digest == record['sha']) if category == 'must' else (data == zero)
            if category == 'must':
                relation = '<='
                if ok:
                    disposition = 'present byte-exact'
                else:
                    disposition = 'ABSENT' if data == zero else 'DIFFERENT'
            else:
                relation = '>'
                disposition = 'absent (zero)' if ok else 'PRESENT'
            raced_messages.append('raced seq %d block %d (LSN inferred H0+1=%d %s L=%d): %s' % (
                record['seq'], block, record['h0'] + 1, relation, boundary, disposition))
        if category == 'must' and digest != record['sha']:
            state = 'absent' if data == zero else 'different'
            bad.append('block %d seq %d: acked with LSN <= %d but %s' % (block, record['seq'], boundary, state))
        elif category == 'lost' and data != zero:
            bad.append('block %d seq %d: LSN > %d but present (image not exact at L)' % (block, record['seq'], boundary))
        elif category in ('straddle', 'lsnless', 'either') and data != zero and digest != record['sha']:
            bad.append('block %d seq %d: neither zero nor its content (torn)' % (block, record['seq']))
        if len(bad) > 20:
            break
    else:
        if f.read(1):
            bad.append('image larger than the %d-block geometry' % blocks)
    info = 'L=%d must=%d lost=%d straddle=%d lsnless=%d failed=%d inflight=%d lost_range=(%d,%d]' % (
        boundary, c['must'], c['lost'], c['straddle'], c['lsnless'], c['failed'], c['inflight'], boundary, max(hmax, boundary))
    if rs is not None:
        detail = raced_messages[0] if raced_messages else 'raced seq %d: block not read' % rs
        info += ' ' + detail
    if bad:
        verdict = 'FAIL'
    elif c['straddle'] or c['lsnless'] or not (c['must'] + c['lost']):
        verdict = 'NOT_COUNTED'
    else:
        verdict = 'PASS'
    return {'verdict': verdict, 'counts': c, 'errors': bad, 'info': info}
