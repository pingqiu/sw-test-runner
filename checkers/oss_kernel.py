"""Pure checks over complete, bound kernel observations supplied by a producer."""
import re


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


def root_names_exist(expected_heads, heads, objects):
    """K6: prove existence in the declared scope, not flatness or image content."""
    pending = []
    unknown = set()
    checked = set()
    missing = set()
    if (not isinstance(expected_heads, list) or not expected_heads
            or any(not isinstance(name, str) or not name for name in expected_heads)
            or not isinstance(heads, dict) or not isinstance(objects, dict)):
        return {'verdict': 'UNKNOWN', 'checked': [], 'missing': [],
                'unknown': ['invalid scope or observation maps']}
    if len(set(expected_heads)) != len(expected_heads):
        unknown.add('duplicate HEAD scope')
    if set(heads) - set(expected_heads):
        unknown.add('unexpected HEAD observation')
    for volume in expected_heads:
        head = heads.get(volume)
        names = _references(head, 'roots')
        if names is None:
            unknown.add(volume)
            continue
        if head['exists'] and head.get('state') not in ('active', 'detached', 'deleting', 'deleted'):
            unknown.add(volume)
            continue
        if head['exists'] and head['state'] == 'deleting':
            continue
        pending.extend(names)
    while pending:
        name = pending.pop()
        if name in checked:
            continue
        checked.add(name)
        observation = objects.get(name)
        names = _references(observation, 'references')
        if names is None:
            unknown.add(name)
            continue
        if not observation['exists']:
            missing.add(name)
            continue
        pending.extend(names)
    verdict = 'FAIL' if missing else 'UNKNOWN' if unknown else 'PASS'
    return {'verdict': verdict, 'checked': sorted(checked),
            'missing': sorted(missing), 'unknown': sorted(unknown)}


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


def entries_data_acknowledged(scope, observation):
    """K8: judge new-entry visibility against causally prior matching data ACKs."""
    try:
        expected, events = _causal_events(scope, observation, 'entries')
    except ValueError as error:
        return {'verdict': 'UNKNOWN', 'checked': [], 'violations': [], 'unknown': [str(error)]}
    for event in events:
        if (event.get('kind') not in ('data_ack', 'entry_visible')
                or event.get('name') not in expected
                or not isinstance(event.get('data_id'), str) or not event['data_id']
                or type(event.get('length')) is not int or event['length'] < 0
                or not isinstance(event.get('sha256'), str)
                or not re.fullmatch('[0-9a-f]{64}', event['sha256'])):
            return {'verdict': 'UNKNOWN', 'checked': [], 'violations': [],
                    'unknown': ['invalid event at ordinal %d' % event['ordinal']]}
    acknowledgements = set()
    checked = set()
    violations = []
    for event in observation['events']:
        identity = (event['name'], event['data_id'], event['length'], event['sha256'])
        if event['kind'] == 'data_ack':
            acknowledgements.add(identity)
        elif event['kind'] == 'entry_visible':
            checked.add(event['name'])
            if identity not in acknowledgements:
                violations.append({'name': event['name'], 'ordinal': event['ordinal'],
                                   'reason': 'no prior matching data ACK'})
    unknown = sorted(set(expected) - checked)
    verdict = 'FAIL' if violations else 'UNKNOWN' if unknown else 'PASS'
    return {'verdict': verdict, 'checked': sorted(checked),
            'violations': violations, 'unknown': unknown}


def _valid_activity(activity):
    if not isinstance(activity, dict):
        return False
    if activity.get('kind') == 'idle':
        return True
    return (activity.get('kind') in ('snap', 'clean')
            and isinstance(activity.get('session'), str) and bool(activity['session']))


def snapshot_removals_safe(scope, observation):
    """K9 only: HEAD snapshot activity excludes immutable-name removal."""
    try:
        expected, events = _causal_events(scope, observation, 'names')
        if not _valid_activity(observation.get('initial_activity')):
            raise ValueError('initial HEAD activity is unknown')
        for event in events:
            if event.get('kind') == 'head_activity':
                if not _valid_activity(event.get('activity')):
                    raise ValueError('HEAD activity is unknown')
            elif event.get('kind') != 'remove_name' or event.get('name') not in expected:
                raise ValueError('unknown removal event')
    except ValueError as error:
        return {'verdict': 'UNKNOWN', 'checked': [], 'violations': [], 'unknown': [str(error)]}
    activity = observation['initial_activity']
    checked = set()
    violations = []
    for event in events:
        if event['kind'] == 'head_activity':
            activity = event['activity']
        elif event['kind'] == 'remove_name':
            checked.add(event['name'])
            if activity['kind'] == 'snap':
                violations.append({'name': event['name'], 'ordinal': event['ordinal'],
                                   'reason': 'HEAD snapshot activity is open'})
    unknown = sorted(set(expected) - checked)
    verdict = 'FAIL' if violations else 'UNKNOWN' if unknown else 'PASS'
    return {'verdict': verdict, 'checked': sorted(checked),
            'violations': violations, 'unknown': unknown}


def acknowledged_writes_synced(scope, observation):
    """K2: match guest ACKs to local records covered by a prior completed sync."""
    try:
        expected, events = _causal_events(scope, observation, 'writes')
        if not isinstance(scope['binding'].get('process'), str) or not scope['binding']['process']:
            raise ValueError('process incarnation is missing')
        appended = set()
        for event in events:
            if event.get('kind') == 'log_sync':
                if type(event.get('through_lsn')) is not int or event['through_lsn'] < 0:
                    raise ValueError('unknown completed sync position')
                continue
            if (event.get('kind') not in ('log_append', 'guest_ack')
                    or event.get('write_id') not in expected
                    or type(event.get('lsn')) is not int or event['lsn'] < 0
                    or not isinstance(event.get('sha256'), str)
                    or not re.fullmatch('[0-9a-f]{64}', event['sha256'])):
                raise ValueError('invalid log/ACK event')
            if event['kind'] == 'log_append':
                if event['write_id'] in appended:
                    raise ValueError('duplicate scoped append')
                appended.add(event['write_id'])
    except ValueError as error:
        return {'verdict': 'UNKNOWN', 'checked': [], 'violations': [], 'unknown': [str(error)]}
    records = set()
    durable = set()
    checked = set()
    violations = []
    for event in events:
        if event['kind'] == 'log_sync':
            durable.update(record for record in records if record[1] <= event['through_lsn'])
            continue
        record = (event['write_id'], event['lsn'], event['sha256'])
        if event['kind'] == 'log_append':
            records.add(record)
        elif event['kind'] == 'guest_ack':
            checked.add(event['write_id'])
            if record not in durable:
                violations.append({'write_id': event['write_id'], 'ordinal': event['ordinal'],
                                   'reason': 'ACK lacks a matching synced log record'})
    unknown = sorted(set(expected) - checked)
    verdict = 'FAIL' if violations else 'UNKNOWN' if unknown else 'PASS'
    return {'verdict': verdict, 'checked': sorted(checked),
            'violations': violations, 'unknown': unknown}


def _valid_holder(value):
    return (isinstance(value, dict) and type(value.get('epoch')) is int and value['epoch'] > 0
            and isinstance(value.get('holder'), str) and bool(value['holder']))


def _head_update_events(scope, observation):
    expected, events = _causal_events(scope, observation, 'updates')
    if not isinstance(scope.get('head'), str) or not scope['head']:
        raise ValueError('HEAD entry identity is missing')
    seen = set()
    for event in events:
        if (event.get('kind') != 'head_update' or event.get('update_id') not in expected
                or event['update_id'] in seen):
            raise ValueError('unknown or duplicate HEAD transition')
        seen.add(event['update_id'])
        if not all(_valid_holder(event.get(field)) for field in ('authority', 'writer', 'before', 'after')):
            raise ValueError('HEAD or authority identity is unknown')
        if not all(isinstance(event[field].get('generation'), str) and event[field]['generation']
                   for field in ('before', 'after')):
            raise ValueError('HEAD generation is unknown')
        writes = event.get('writes')
        if not isinstance(writes, list) or not writes:
            raise ValueError('applied mutation evidence is missing')
        for write in writes:
            if (not isinstance(write, dict)
                    or not isinstance(write.get('entry'), str) or not write['entry']
                    or write.get('operation') not in ('conditional_update', 'unconditional_update',
                                                       'exclusive_create', 'delete')
                    or 'expected_generation' not in write or 'expected_epoch' not in write):
                raise ValueError('applied mutation or preconditions are unknown')
            generation, epoch = write['expected_generation'], write['expected_epoch']
            if (generation is not None and (not isinstance(generation, str) or not generation)
                    or epoch is not None and (type(epoch) is not int or epoch < 0)):
                raise ValueError('invalid wire precondition')
    return expected, events


def head_updates_conditional(scope, observation):
    """K7: one existing-HEAD conditional mutation under the observed authority."""
    try:
        expected, events = _head_update_events(scope, observation)
    except ValueError as error:
        return {'verdict': 'UNKNOWN', 'checked': [], 'violations': [], 'unknown': [str(error)]}
    checked = set()
    violations = []
    for event in events:
        before, after = event['before'], event['after']
        writer, authority = event['writer'], event['authority']
        writes = event['writes']
        reasons = []
        if len(writes) != 1:
            reasons.append('not one applied entry mutation')
        else:
            write = writes[0]
            if write['entry'] != scope['head'] or write['operation'] != 'conditional_update':
                reasons.append('not a conditional update of this HEAD')
            if write['expected_generation'] != before['generation']:
                reasons.append('generation precondition does not match')
            if write['expected_epoch'] != before['epoch']:
                reasons.append('epoch precondition does not match')
        if (writer['epoch'], writer['holder']) != (authority['epoch'], authority['holder']):
            reasons.append('writer is not the current authority')
        if (after['epoch'], after['holder']) != (writer['epoch'], writer['holder']):
            reasons.append('result does not belong to the writer')
        if (writer['epoch'] < before['epoch']
                or writer['epoch'] == before['epoch'] and writer['holder'] != before['holder']):
            reasons.append('epoch does not permit this holder')
        if after['generation'] == before['generation']:
            reasons.append('mutation did not advance the generation')
        checked.add(event['update_id'])
        if reasons:
            violations.append({'update_id': event['update_id'], 'ordinal': event['ordinal'],
                               'reasons': reasons})
    unknown = sorted(set(expected) - checked)
    verdict = 'FAIL' if violations else 'UNKNOWN' if unknown else 'PASS'
    return {'verdict': verdict, 'checked': sorted(checked),
            'violations': violations, 'unknown': unknown}
