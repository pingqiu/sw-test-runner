"""cloud_batch assess with replication 001: every volume has exactly R owners."""
import json, pathlib, sys, tempfile, unittest
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import cloud_batch

HOSTS = {'vol%d' % i: '10.84.1.%d:8080' % (20 + i) for i in range(1, 7)}


def build(copies, drop_copy=False, owner_override=None):
    root = pathlib.Path(tempfile.mkdtemp())
    hosts = sorted(HOSTS)
    placement = {str(v): [hosts[(v - 1) % 6], hosts[v % 6]][:copies] for v in range(1, 7)}
    if drop_copy:
        placement['6'] = placement['6'][:1]
    nodes = [{'Url': HOSTS[h]} for h in hosts]
    for phase, size in (('before', 0), ('after', 1)):
        d = root / phase / 'meta'
        d.mkdir(parents=True)
        (d / 'status.json').write_text(json.dumps({'Topology': {'DataCenters': [{'Racks': [{'DataNodes': nodes}]}]}}))
        for h in hosts:
            vols = [dict(Id=int(v), Size=size * 1048576, FileCount=size * 10, ReadOnly=False) for v, owners in placement.items() if h in owners]
            (root / phase / h).mkdir(parents=True)
            (root / phase / h / 'status.json').write_text(json.dumps({'Volumes': vols}))
    for client in ('client1', 'client2', 'client3'):
        rows = []
        for v, owners in placement.items():
            urls = sorted(HOSTS[h] for h in owners)
            if owner_override and v == '1':
                urls = owner_override
            rows.append(dict(event='OWNER', volume=v, url=urls[0]) if copies == 1 else dict(event='OWNER', volume=v, urls=urls))
        for k in range(512):
            for e in ('ACK', 'EXACT'):
                rows.append(dict(event=e, key='%s-%d' % (client, k), length=1048576, sha256='a' * 64, fids=['%d,ab' % (k % 6 + 1)]))
        for k in range(32):
            for e in ('S3_ACK', 'S3_EXACT'):
                rows.append(dict(event=e, key='%s-s3-%d' % (client, k), length=4194304, sha256='b' * 64))
        (root / 'load' / client).mkdir(parents=True)
        (root / 'load' / client / 'cloud-kv.jsonl').write_text('\n'.join(json.dumps(r) for r in rows))
    return root


class ReplicationAssess(unittest.TestCase):
    def test_two_copies_on_different_owners_pass(self):
        cloud_batch.assess(build(2), HOSTS, 2)

    def test_default_single_copy_unchanged(self):
        cloud_batch.assess(build(1), HOSTS)

    def test_missing_copy_refuses(self):
        with self.assertRaises(AssertionError):
            cloud_batch.assess(build(2, drop_copy=True), HOSTS, 2)

    def test_two_copies_refuse_under_000(self):
        with self.assertRaisesRegex(AssertionError, 'duplicate volume owner'):
            cloud_batch.assess(build(2), HOSTS)

    def test_client_lookup_must_name_both_copies(self):
        with self.assertRaises(AssertionError):
            cloud_batch.assess(build(2, owner_override=[HOSTS['vol1']]), HOSTS, 2)


if __name__ == '__main__':
    unittest.main()
