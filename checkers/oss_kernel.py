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


def entries_data_acknowledged(scope, observation):
    """K8: judge new-entry visibility against causally prior matching data ACKs."""
    if not isinstance(scope, dict) or not isinstance(observation, dict):
        return {'verdict': 'UNKNOWN', 'checked': [], 'violations': [],
                'unknown': ['invalid scope or observation']}
    binding = scope.get('binding')
    expected = scope.get('entries')
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
        return {'verdict': 'UNKNOWN', 'checked': [], 'violations': [],
                'unknown': ['incomplete or unbound trace']}
    for ordinal, event in enumerate(events):
        if (not isinstance(event, dict)
                or type(event.get('ordinal')) is not int or event['ordinal'] != ordinal
                or event.get('kind') not in ('data_ack', 'entry_visible')
                or event.get('name') not in expected
                or not isinstance(event.get('data_id'), str) or not event['data_id']
                or type(event.get('length')) is not int or event['length'] < 0
                or not isinstance(event.get('sha256'), str)
                or not re.fullmatch('[0-9a-f]{64}', event['sha256'])):
            return {'verdict': 'UNKNOWN', 'checked': [], 'violations': [],
                    'unknown': ['invalid event at ordinal %d' % ordinal]}
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
