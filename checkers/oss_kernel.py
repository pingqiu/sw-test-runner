"""Pure checks over complete, bound kernel observations supplied by a producer."""


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
        names = _references(heads.get(volume), 'roots')
        if names is None:
            unknown.add(volume)
        else:
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
