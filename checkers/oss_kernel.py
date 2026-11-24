"""Pure checks over complete, bound kernel observations supplied by a producer."""
import re
import hashlib
from copy import deepcopy


def _references(observation, field):
    if not isinstance(observation, dict):
        return None
    status = observation.get('producer_exit')
    exists = observation.get('exists')
    names = observation.get(field)
    if type(status) is not int or status != 0 or type(exists) is not bool:
        return None
    if not isinstance(names, list) or any(
            not isinstance(name, str) or not name for name in names):
        return None
    if not exists and names:
        return None
    return names


def _head_roots(head):
    if (not isinstance(head, dict) or type(head.get('producer_exit')) is not int
            or head['producer_exit'] != 0 or type(head.get('exists')) is not bool):
        return None
    if head['exists']:
        if head.get('state') not in ('active', 'detached', 'deleting', 'deleted'):
            return None
        if head['state'] in ('deleting', 'deleted'):
            return []
    if 'current' not in head or not isinstance(head.get('snapshots'), dict):
        return None
    current, snapshots = head['current'], head['snapshots']
    if current is not None and (not isinstance(current, str) or not current):
        return None
    if not head['exists'] and (current is not None or snapshots):
        return None
    if any(not isinstance(name, str) or not name for name in snapshots):
        return None
    roots = [] if current is None else [current]
    for snapshot in snapshots.values():
        if (not isinstance(snapshot, dict) or not isinstance(snapshot.get('root'), str)
                or not snapshot['root']):
            return None
        roots.append(snapshot['root'])
    return roots


def _read_observation(reader, name):
    try:
        return deepcopy(reader(name))
    except Exception:
        return None


def _head_version(head):
    if _head_roots(head) is None:
        return None
    if head['exists'] and (not isinstance(head.get('generation'), str) or not head['generation']):
        return None
    return {key: head.get(key) for key in ('exists', 'generation', 'state', 'current', 'snapshots')}


def _inspect_names(names, read_object):
    pending = list(names)
    checked, missing, unknown = set(), set(), set()
    while pending:
        name = pending.pop()
        if name in checked:
            continue
        checked.add(name)
        observation = _read_observation(read_object, name)
        references = _references(observation, 'references')
        if references is None:
            unknown.add(name)
            continue
        if not observation['exists']:
            missing.add(name)
            continue
        pending.extend(references)
    return checked, missing, unknown


def root_names_exist(expected_heads, read_head, read_object, max_attempts=3):
    """K6 at a stable filer HEAD generation; changed observations are retried."""
    checked, missing, unknown = set(), set(), set()
    generations = {}
    if (not isinstance(expected_heads, list) or not expected_heads
            or any(not isinstance(name, str) or not name for name in expected_heads)
            or not callable(read_head) or not callable(read_object)
            or type(max_attempts) is not int or max_attempts < 1):
        return {'verdict': 'UNKNOWN', 'checked': [], 'missing': [],
                'unknown': ['invalid scope or filer readers'], 'generations': {}}
    if len(set(expected_heads)) != len(expected_heads):
        unknown.add('duplicate HEAD scope')
    for volume in dict.fromkeys(expected_heads):
        for attempt in range(max_attempts):
            head = _read_observation(read_head, volume)
            before = _head_version(head)
            if before is None:
                unknown.add(volume)
                break
            attempt_checked, attempt_missing, attempt_unknown = _inspect_names(
                _head_roots(head), read_object)
            after = _head_version(_read_observation(read_head, volume))
            if after is None:
                unknown.add(volume)
                break
            if before != after:
                if attempt + 1 == max_attempts:
                    unknown.add(volume)
                continue
            generations[volume] = before['generation']
            checked.update(attempt_checked)
            missing.update(attempt_missing)
            unknown.update(attempt_unknown)
            break
    verdict = 'FAIL' if missing else 'UNKNOWN' if unknown else 'PASS'
    return {'verdict': verdict, 'checked': sorted(checked),
            'missing': sorted(missing), 'unknown': sorted(unknown), 'generations': generations}


def _causal_events(scope, observation, scope_field):
    """Validate the identical capture envelope used by ordered-event facts."""
    if not isinstance(scope, dict) or not isinstance(observation, dict):
        raise ValueError('invalid scope or observation')
    binding = scope.get('binding')
    expected = scope.get(scope_field)
    events = observation.get('events')
    status = observation.get('producer_exit')
    coverage = observation.get('coverage')
    valid = (
        isinstance(binding, dict)
        and all(isinstance(binding.get(key), str) and binding[key] for key in ('run', 'volume'))
        and observation.get('binding') == binding
        and isinstance(expected, list) and bool(expected)
        and all(isinstance(name, str) and name for name in expected)
        and len(set(expected)) == len(expected)
        and type(status) is int and status == 0
        and isinstance(events, list) and bool(events)
        and isinstance(coverage, dict)
        and all(type(value) is int for value in coverage.values())
        and coverage == {'first': 0, 'last': len(events) - 1, 'count': len(events)})
    if not valid:
        raise ValueError('incomplete or unbound trace')
    for ordinal, event in enumerate(events):
        if (not isinstance(event, dict) or type(event.get('ordinal')) is not int
                or event['ordinal'] != ordinal):
            raise ValueError('invalid causal ordinal %d' % ordinal)
    return expected, events


def entries_match_names(volume, list_entries, read_entry):
    """K8: fully readable filer files have the complete SHA-256 in their names."""
    checked, violations, unknown = set(), [], []
    if (not isinstance(volume, str) or not volume or volume in ('.', '..')
            or any(char in volume for char in '/\\\x00')
            or not callable(list_entries) or not callable(read_entry)):
        return {'verdict': 'UNKNOWN', 'checked': [], 'violations': [], 'unknown': ['invalid readers or volume']}
    for namespace in ('pool', 'seg', 'root'):
        prefix = volume + '/' + namespace + '/'
        names = _read_observation(list_entries, prefix)
        if not isinstance(names, list):
            unknown.append(prefix)
            continue
        seen = set()
        for name in names:
            if (not isinstance(name, str) or not name.startswith(prefix)
                    or any(part in ('', '.', '..') for part in name.split('/'))
                    or '\\' in name or '\x00' in name or name in seen):
                unknown.append(prefix + ' invalid inventory')
                continue
            seen.add(name)
            suffix = name[len(prefix):]
            patterns = {'pool': r'[^/]+/[0-9]+-[0-9a-f]{64}',
                        'seg': r'[^/]+-[0-9a-f]{64}', 'root': r'[0-9a-f]{64}'}
            if not re.fullmatch(patterns[namespace], suffix):
                violations.append({'name': name, 'reason': 'invalid content-address name'})
                continue
            record = _read_observation(read_entry, name)
            if (not isinstance(record, dict) or type(record.get('producer_exit')) is not int
                    or record['producer_exit'] != 0 or record.get('exists') is not True
                    or type(record.get('size')) is not int or record['size'] < 0
                    or not isinstance(record.get('content'), bytes)):
                unknown.append(name)
                continue
            data, size = record['content'], record['size']
            expected_sha = name.rsplit('/', 1)[-1].rsplit('-', 1)[-1]
            checked.add(name)
            if len(data) != size:
                violations.append({'name': name, 'reason': 'incomplete filer content'})
            if hashlib.sha256(data).hexdigest() != expected_sha:
                violations.append({'name': name, 'reason': 'content does not match name'})
    return {'verdict': 'FAIL' if violations else 'UNKNOWN' if unknown else 'PASS',
            'checked': sorted(checked), 'violations': violations, 'unknown': sorted(set(unknown))}


def _valid_activity(activity):
    if not isinstance(activity, dict):
        return False
    if activity.get('kind') == 'idle':
        return True
    return (activity.get('kind') in ('snap', 'clean')
            and isinstance(activity.get('session'), str) and bool(activity['session']))


def snapshot_removals_safe(scope, observation):
    """K9: filer metadata order excludes pool deletion during HEAD snapshot activity."""
    try:
        expected, events = _causal_events(scope, observation, 'names')
        if not _valid_activity(observation.get('initial_activity')):
            raise ValueError('initial HEAD activity is unknown')
        volume = scope['binding']['volume']
        if (observation.get('source') != 'filer_metadata'
                or not isinstance(observation.get('stream_id'), str) or not observation['stream_id']
                or not isinstance(observation.get('initial_generation'), str)
                or not observation['initial_generation']
                or any(not name.startswith(volume + '/pool/') for name in expected)):
            raise ValueError('metadata stream or pool scope is unknown')
        generation = observation['initial_generation']
        for event in events:
            if event.get('kind') == 'head_update':
                if (not _valid_activity(event.get('activity'))
                        or event.get('entry') != volume + '/HEAD'
                        or event.get('before_generation') != generation
                        or not isinstance(event.get('generation'), str)
                        or not event['generation'] or event['generation'] == generation):
                    raise ValueError('HEAD activity is unknown')
                generation = event['generation']
            elif (event.get('kind') != 'delete_entry'
                    or not isinstance(event.get('name'), str)
                    or not event['name'].startswith(volume + '/')
                    or '\\' in event['name'] or '\x00' in event['name']
                    or any(part in ('', '.', '..') for part in event['name'].split('/'))):
                raise ValueError('unknown removal event')
    except ValueError as error:
        return {'verdict': 'UNKNOWN', 'checked': [], 'violations': [], 'unknown': [str(error)]}
    activity = observation['initial_activity']
    checked = set()
    violations = []
    for event in events:
        if event['kind'] == 'head_update':
            activity = event['activity']
        elif event['kind'] == 'delete_entry' and event['name'].startswith(volume + '/pool/'):
            checked.add(event['name'])
            if activity['kind'] == 'snap':
                violations.append({'name': event['name'], 'ordinal': event['ordinal'],
                                   'reason': 'HEAD snapshot activity is open'})
    unknown = sorted(set(expected) - checked)
    verdict = 'FAIL' if violations else 'UNKNOWN' if unknown else 'PASS'
    return {'verdict': verdict, 'checked': sorted(checked),
            'violations': violations, 'unknown': unknown}


def _valid_head_identity(value):
    return (isinstance(value, dict) and type(value.get('epoch')) is int and value['epoch'] > 0
            and isinstance(value.get('holder'), str) and bool(value['holder'])
            and type(value.get('generation')) is int and value['generation'] >= 0)


def head_updates_ordered(scope, observation):
    """K7: filer HEAD generations and authority-store grants, never clock order."""
    try:
        expected, events = _causal_events(scope, observation, 'updates')
        if (scope.get('head') != scope['binding']['volume'] + '/HEAD'
                or observation.get('source') != 'filer_metadata'
                or not isinstance(observation.get('stream_id'), str) or not observation['stream_id']
                or not _valid_head_identity(observation.get('initial_head'))):
            raise ValueError('existing HEAD metadata boundary is missing')
        store = observation.get('authority_store')
        if (not isinstance(store, dict) or store.get('binding') != scope['binding']
                or store.get('source') != 'authority_store'
                or type(store.get('producer_exit')) is not int or store['producer_exit'] != 0
                or not isinstance(store.get('records'), list) or not store['records']):
            raise ValueError('authority-store capture is missing')
        grants = {}
        previous_revision = 0
        for record in store['records']:
            if (not isinstance(record, dict) or type(record.get('revision')) is not int
                    or record['revision'] <= previous_revision
                    or type(record.get('epoch')) is not int or record['epoch'] < 1
                    or not isinstance(record.get('holder'), str) or not record['holder']):
                raise ValueError('authority records are not in store revision order')
            previous_revision = record['revision']
            grants[previous_revision] = record
        seen = set()
        for event in events:
            if (event.get('kind') not in ('head_update', 'delete_entry', 'create_entry')
                    or event.get('entry') != scope['head']
                    or event.get('update_id') not in expected or event['update_id'] in seen):
                raise ValueError('unknown or duplicate HEAD metadata event')
            seen.add(event['update_id'])
            if event['kind'] == 'head_update' and not all(
                    _valid_head_identity(event.get(field)) for field in ('before', 'after')):
                raise ValueError('HEAD before/after body is unknown')
    except ValueError as error:
        return {'verdict': 'UNKNOWN', 'checked': [], 'violations': [], 'unknown': [str(error)]}
    current = observation['initial_head']
    checked, violations, unknown = [], [], []
    last_authority_revision = 0
    for event in events:
        update_id = event['update_id']
        checked.append(update_id)
        reasons = []
        if event['kind'] == 'delete_entry':
            current = None
        elif event['kind'] == 'create_entry':
            reasons.append('HEAD recreated for an existing volume')
            current = None
        else:
            before, after = event['before'], event['after']
            if current is None:
                reasons.append('HEAD updated after deletion')
            elif before != current:
                unknown.append(update_id + ': metadata capture is discontinuous')
            if current is not None and after['generation'] != current['generation'] + 1:
                reasons.append('metadata stream repeated or skipped a generation')
            if after['generation'] != before['generation'] + 1:
                reasons.append('generation did not advance by exactly one')
            if after['epoch'] < before['epoch']:
                reasons.append('HEAD epoch decreased')
            revision = event.get('authority_revision_before')
            if (type(revision) is not int or revision not in grants
                    or revision < last_authority_revision):
                unknown.append(update_id + ': prior authority-store join is missing or reversed')
            else:
                last_authority_revision = revision
                grant = grants[revision]
                if (after['epoch'], after['holder']) != (grant['epoch'], grant['holder']):
                    reasons.append('HEAD does not match the previously granted line')
            current = after
        if reasons:
            violations.append({'update_id': update_id, 'ordinal': event['ordinal'], 'reasons': reasons})
    unknown.extend(sorted(set(expected) - set(checked)))
    return {'verdict': 'FAIL' if violations else 'UNKNOWN' if unknown else 'PASS',
            'checked': checked, 'violations': violations, 'unknown': unknown}
