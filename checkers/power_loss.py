"""Content facts for a recovered image and its post-recovery publication."""
import hashlib
import re


MAX_CHUNK_BYTES = 64 * 1024 * 1024
# Two 100 ms flusher periods plus the reviewed 50 ms observation margin.
FIXED_REVISION_WAIT_NS = 250_000_000


def _require(condition, message):
    if not condition:
        raise ValueError(message)


def _number(value, minimum=0):
    return type(value) is int and value >= minimum


def _full_sha(value):
    return isinstance(value, str) and re.fullmatch('[0-9a-f]{64}', value) is not None


def _validate_observation(case, observation, source, written):
    block_bytes, chunk_bytes, size = (case[k] for k in ('block_bytes', 'chunk_bytes', 'volume_bytes'))
    count = (size + chunk_bytes - 1) // chunk_bytes
    binding = {k: case[k] for k in ('run_id', 'image_id', 'volume_id', 'revision')}
    binding['view_id'] = case[source + '_view_id']
    _require(observation['binding'] == binding and observation['source'] == source, 'wrong view binding')
    _require(type(observation['producer_exit']) is int and observation['producer_exit'] == 0, 'producer failed')
    expected = {'chunks': count, 'count': count, 'bytes': size, 'first': 0, 'last': size - 1}
    _require(observation['coverage'] == expected and
             all(type(v) is int for v in observation['coverage'].values()), 'incomplete coverage')
    _require(source != 'published' or observation['snapshot'] == case['snapshot'], 'wrong snapshot binding')
    chunks = {}
    for record in observation['chunks']:
        index = record['chunk_index']
        _require(_number(index) and index < count and index not in chunks, 'duplicate or out-of-range chunk')
        length = min(chunk_bytes, size - index * chunk_bytes)
        _require(type(record['offset']) is int and record['offset'] == index * chunk_bytes and
                 type(record['length']) is int and record['length'] == length, 'wrong chunk range')
        _require(record['binding'] == binding and record['source'] == source, 'wrong chunk binding')
        _require(_full_sha(record['sha256']), 'full SHA256 required')
        if 'data' in record:
            _require(isinstance(record['data'], bytes) and len(record['data']) == length and
                     _sha(record['data']) == record['sha256'], 'raw chunk/hash disagreement')
        chunks[index] = record
    _require(len(chunks) == count, 'missing chunks')
    blocks = {}
    for record in observation['blocks']:
        block = record['block']
        _require(_number(block) and block in written and block not in blocks, 'wrong or duplicate written block')
        data = record['data']
        _require(isinstance(data, bytes) and len(data) == block_bytes and
                 _full_sha(record['sha256']) and _sha(data) == record['sha256'], 'raw block/hash disagreement')
        chunk = chunks[block * block_bytes // chunk_bytes]
        if 'data' in chunk:
            offset = block * block_bytes - chunk['offset']
            _require(chunk['data'][offset:offset + block_bytes] == data, 'block/chunk disagreement')
        blocks[block] = data
    _require(set(blocks) == written, 'missing written blocks')
    return chunks, blocks


def _validate_inputs(case, workload, local, published):
    block_bytes, chunk_bytes, size = (case[k] for k in ('block_bytes', 'chunk_bytes', 'volume_bytes'))
    _require(all(_number(v, 1) for v in (block_bytes, chunk_bytes, size)) and
             size % block_bytes == 0 and chunk_bytes % block_bytes == 0 and
             chunk_bytes <= MAX_CHUNK_BYTES, 'unsupported geometry')
    for key in ('run_id', 'image_id', 'volume_id', 'revision', 'local_view_id', 'published_view_id'):
        _require(isinstance(case[key], str) and bool(case[key]), 'missing identity')
    _require(case['local_view_id'] != case['published_view_id'], 'views must be distinct')
    window = case['window']
    order = [window[k] for k in ('recovery_complete', 'local_complete', 'snapshot_complete')]
    _require(all(_number(v) for v in order) and order[0] < order[1] < order[2] and
             type(window['guest_writes']) is int and window['guest_writes'] == 0,
             'publication must follow recovery/local reads without intervening guest writes')
    snapshot = case['snapshot']
    _require(isinstance(snapshot['attempt'], str) and bool(snapshot['attempt']) and
             _number(snapshot['head_gen'], 1) and _number(snapshot['cut_lsn']), 'missing snapshot identity')
    _require(bool(workload['operations']) and
             set(workload['operations']) <= {'write', 'flush', 'fua'}, 'unsupported workload operation')
    for block, data in workload['baseline'].items():
        _require(_number(block) and block < size // block_bytes and
                 isinstance(data, bytes) and len(data) == block_bytes, 'invalid baseline block')
    written = set()
    for write in workload['writes']:
        block = write['block']
        _require(_number(block) and block < size // block_bytes and block not in written, 'repeated or invalid write')
        _require(type(write['flushed']) is bool and isinstance(write['new'], bytes) and
                 len(write['new']) == block_bytes, 'invalid write obligation')
        written.add(block)
    written.update(workload['baseline'])
    local_chunks, local_blocks = _validate_observation(case, local, 'local', written)
    published_chunks, published_blocks = _validate_observation(case, published, 'published', written)
    for block in written:
        index = block * block_bytes // chunk_bytes
        if local_chunks[index]['sha256'] == published_chunks[index]['sha256']:
            _require(local_blocks[block] == published_blocks[block], 'equal chunk hashes but different raw blocks')


def _sha(data):
    return hashlib.sha256(data).hexdigest()


def _campaign_tick(case):
    try:
        proof = case['proof']
        _require(type(case['write_ahead_fixed']) is bool, 'revision policy missing')
        binding = {k: case[k] for k in ('run_id', 'image_id', 'volume_id', 'revision')}
        _require(proof['binding'] == binding, 'wrong tick image binding')
        _require(proof['revision'] == case['revision'] and isinstance(proof['clock_id'], str) and
                 bool(proof['clock_id']), 'tick proof binding missing')
        start, end, count = (proof[k] for k in ('write_ack_ns', 'cut_mark_ns', 'extent_writes'))
        _require(_number(start) and _number(end) and end > start and _number(count), 'invalid tick interval')
        if case['write_ahead_fixed']:
            if count > 0:
                return {'verdict': 'FAIL', 'labels': ['WRITE-AHEAD-VIOLATION'],
                        'extent_writes': count}
            poll = proof['status_poll_ns']
            _require(end - start >= FIXED_REVISION_WAIT_NS and _number(poll) and start < poll < end and
                     type(proof['status_exit']) is int and proof['status_exit'] == 0, 'fixed-revision tick unproven')
        else:
            instant = proof['extent_write_ns']
            _require(count > 0 and _number(instant) and start < instant < end, 'extent-write window unproven')
        return {'verdict': 'PASS', 'labels': [], 'extent_writes': count}
    except (KeyError, TypeError, ValueError) as error:
        return {'verdict': 'UNKNOWN', 'labels': [], 'errors': [str(error)]}


def _compare_images(case, workload, local, published):
    """Compare complete images; no host actions or crash construction occur here.

    The baseline is sparse, with zero in every undeclared block. Unflushed
    replacements must recover as a prefix of guest issue order, with OLD after
    the first gap; every flushed replacement must be NEW.
    The published view must name the snapshot taken after these local reads,
    with no intervening guest writes. Producers supply full SHA256 observations.
    """
    block_bytes = case['block_bytes']
    chunk_bytes = case['chunk_bytes']
    volume_bytes = case['volume_bytes']
    local_chunks = {r['chunk_index']: r for r in local['chunks']}
    published_chunks = {r['chunk_index']: r for r in published['chunks']}
    local_blocks = {r['block']: r['data'] for r in local['blocks']}
    baseline = workload['baseline']
    writes = {r['block']: r for r in workload['writes']}
    history_labels, history_errors, divergence = set(), [], []
    raw_missing = []
    zero = bytes(block_bytes)
    for block, old in baseline.items():
        if block not in writes and local_blocks[block] != old:
            history_labels.add('ACKED-LOST')
            history_errors.append({'block': block, 'reason': 'flushed baseline value lost'})
    earlier_old = None
    for block, write in writes.items():
        actual = local_blocks[block]
        old = baseline.get(block, zero)
        if write['flushed'] and actual != write['new']:
            history_labels.add('ACKED-LOST')
            history_errors.append({'block': block, 'reason': 'flushed value lost'})
        elif not write['flushed'] and actual not in (old, write['new']):
            history_labels.add('UNEXPECTED')
            history_errors.append({'block': block, 'reason': 'neither OLD nor NEW'})
        if not write['flushed'] and old != write['new']:
            if earlier_old is not None and actual == write['new']:
                history_labels.add('NON_PREFIX')
                history_errors.append({'block': block, 'earlier_old': earlier_old,
                                       'reason': 'recovery replayed beyond an earlier gap'})
            if actual == old and earlier_old is None:
                earlier_old = block

    chunks = (volume_bytes + chunk_bytes - 1) // chunk_bytes
    expected_overrides = dict(baseline)
    expected_overrides.update(local_blocks)
    by_chunk = {}
    for block, data in expected_overrides.items():
        by_chunk.setdefault(block * block_bytes // chunk_bytes, []).append((block, data))
    zero_hashes = {}
    for index in range(chunks):
        length = min(chunk_bytes, volume_bytes - index * chunk_bytes)
        if index in by_chunk:
            expected = bytearray(length)
            for block, data in by_chunk[index]:
                offset = block * block_bytes - index * chunk_bytes
                expected[offset:offset + block_bytes] = data
            expected_sha = _sha(expected)
        else:
            if length not in zero_hashes:
                zero_hashes[length] = _sha(bytes(length))
            expected_sha = zero_hashes[length]
        if local_chunks[index]['sha256'] != expected_sha:
            history_labels.add('UNEXPECTED')
            history_errors.append({'chunk': index, 'reason': 'outside declared history'})
        if local_chunks[index]['sha256'] != published_chunks[index]['sha256']:
            divergence.append(index)
            if 'data' not in local_chunks[index] or 'data' not in published_chunks[index]:
                raw_missing.append(index)

    rows = {
        'LOCAL_HISTORY': {
            'verdict': 'FAIL' if history_labels else 'PASS',
            'labels': [label for label in ('ACKED-LOST', 'UNEXPECTED', 'NON_PREFIX') if label in history_labels],
            'errors': history_errors[:20]},
        'PUBLISHED_EQUALS_LOCAL': {
            'verdict': 'FAIL' if divergence else 'PASS',
            'labels': (['DIVERGED'] if divergence else []) + (['RAW-MISSING'] if raw_missing else []),
            'chunks': divergence[:20], 'raw_missing_chunks': raw_missing[:20]},
    }
    image_verdict = 'FAIL' if history_labels or divergence else 'PASS'
    return {'verdict': image_verdict, 'image_verdict': image_verdict,
            'campaign_verdict': 'PASS', 'rows': rows,
            'counts': {'chunks_compared': chunks, 'blocks_checked': len(local_blocks),
                       'differing_chunks': len(divergence)}}



def _tag_bytes(record):
    tag = record['tag']
    _require(isinstance(tag, str), 'invalid pattern tag')
    if tag == 'zero':
        data = bytes(4096)
    elif tag.startswith('other:'):
        data = record.get('data')
        _require(isinstance(data, bytes) and len(data) == 4096,
                 'other block requires complete raw bytes')
        _require(re.fullmatch('other:[0-9a-f]{32}', tag) is not None and
                 data[:16].hex() == tag[6:], 'other prefix/raw disagreement')
    else:
        encoded = tag.encode('utf-8')
        _require(len(encoded) <= 50 and b'\n' not in encoded and b'\r' not in encoded,
                 'unsupported pattern tag')
        line = (b'WALFS<' + encoded + b'>').ljust(63, b'.') + b'\n'
        data = line * 64
    _require('data' not in record or record['data'] == data, 'pattern/raw disagreement')
    return data


def _tag_views(case, workload, local, published):
    """Rebuild one chunk at a time; only differing chunks retain raw bytes."""
    _require(case.get('observation_purpose') == 'legacy_parity',
             'pattern tags are permitted only for legacy parity, never qualification')
    size, chunk_bytes = case['volume_bytes'], case['chunk_bytes']
    _require(case['block_bytes'] == 4096 and _number(size, 1) and size % 4096 == 0 and
             _number(chunk_bytes, 4096) and chunk_bytes % 4096 == 0 and
             chunk_bytes <= MAX_CHUNK_BYTES, 'unsupported tag geometry')
    count = (size + chunk_bytes - 1) // chunk_bytes
    written = set(workload['baseline']) | {w['block'] for w in workload['writes']}
    converted, by_view, checks = [], [], []
    for view in (local, published):
        _require(view.get('encoding') == 'pattern-tags/v1', 'both tag views required')
        records = {}
        for record in view['tag_blocks']:
            block = record['block']
            _require(_number(block) and block < size // 4096 and block not in records,
                     'duplicate or out-of-range tag block')
            records[block] = _tag_bytes(record)
        chunks = {r['chunk_index']: r for r in view['chunks']}
        _require(len(chunks) == len(view['chunks']) == count and
                 set(chunks) == set(range(count)), 'incomplete tag chunk coverage')
        output = dict(view, chunks=[], blocks=[])
        grouped = {}
        for block, data in records.items():
            grouped.setdefault(block * 4096 // chunk_bytes, []).append((block, data))
        for block in sorted(written):
            data = records.get(block, bytes(4096))
            output['blocks'].append({'block': block, 'sha256': _sha(data), 'data': data})
        converted.append(output)
        by_view.append((chunks, grouped))
    for index in range(count):
        rebuilt = []
        for output, (chunks, grouped) in zip(converted, by_view):
            length = min(chunk_bytes, size - index * chunk_bytes)
            data = bytearray(length)
            nonzero = 0
            for block, raw in grouped.get(index, []):
                offset = block * 4096 - index * chunk_bytes
                data[offset:offset + 4096] = raw
                nonzero += raw != bytes(4096)
            record = dict(chunks[index])
            _require(type(record['nonzero_blocks']) is int and
                     record['nonzero_blocks'] == nonzero, 'nonzero block count disagreement')
            digest = _sha(data)
            observed = record.get('scan_sha256')
            if observed is not None:
                _require(isinstance(observed, str) and
                         re.fullmatch('(?:[0-9a-f]{16}|[0-9a-f]{64})', observed) is not None,
                         'invalid scan hash cross-check')
                if digest[:len(observed)] != observed:
                    checks.append({'source': output['source'], 'chunk': index})
            record['sha256'] = digest
            output['chunks'].append(record)
            rebuilt.append(bytes(data))
        if rebuilt[0] != rebuilt[1]:
            for output, data in zip(converted, rebuilt):
                output['chunks'][-1]['data'] = data
    row = {'verdict': 'FAIL' if checks else 'PASS',
           'labels': ['HARNESS-HASH-MISMATCH'] if checks else [], 'chunks': checks[:20]}
    return converted[0], converted[1], row


def power_loss_image_exact(case, workload, local, published):
    """Return UNKNOWN on incomplete, unsupported or inconsistently bound evidence."""
    try:
        tag_row = None
        if 'encoding' in local or 'encoding' in published:
            local, published, tag_row = _tag_views(case, workload, local, published)
        _validate_inputs(case, workload, local, published)
        result = _compare_images(case, workload, local, published)
        tick = _campaign_tick(case)
        result['rows']['CAMPAIGN_TICK'] = tick
        result['campaign_verdict'] = tick['verdict']
        if result['image_verdict'] != 'FAIL':
            result['verdict'] = tick['verdict']
        if tag_row is not None:
            result['rows']['TAG_SCAN'] = tag_row
            if tag_row['verdict'] == 'FAIL':
                result['verdict'] = 'FAIL'
        result['deciding_evidence'] = {
            'local': dict(local['binding']), 'published': dict(published['binding']),
            'snapshot': dict(case['snapshot']), 'window': dict(case['window']),
        }
        return result
    except (KeyError, TypeError, ValueError, AttributeError) as error:
        rows = {name: {'verdict': 'UNKNOWN', 'labels': []} for name in
                ('LOCAL_HISTORY', 'PUBLISHED_EQUALS_LOCAL', 'CAMPAIGN_TICK')}
        if 'encoding' in local or 'encoding' in published:
            rows['TAG_SCAN'] = {'verdict': 'UNKNOWN', 'labels': [], 'errors': [str(error)]}
        return {'verdict': 'UNKNOWN', 'image_verdict': 'UNKNOWN',
                'campaign_verdict': 'UNKNOWN', 'rows': rows,
                'errors': [str(error)], 'counts': {'chunks_compared': 0, 'blocks_checked': 0,
                                                 'differing_chunks': 0}}
