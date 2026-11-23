"""Artifact-only bindings for the accepted hostloss judge actions.

No SSH, clock sampling, lease operation or counting authority. Call initial
judges first, then render followups from their confirmed receipts. Runtime
registration/execution remains unavailable until the judge pack lands.
"""
import json
import pathlib
import re


def artifact(config, name):
    return str(pathlib.Path(config['root']) / name)


def read(config, name):
    return json.loads(pathlib.Path(artifact(config, name)).read_text())


def action(config, name, **params):
    result = {'action': name, **params}
    if config.get('rehearsal') is True:
        if not re.fullmatch(r'[a-z0-9-]+-r[0-9]+', config.get('run_id', '')) or config.get('release_credit') is not False:
            raise ValueError('NOT_COUNTED exception is rehearsal-only with zero release credit')
        result['allow_not_counted'] = 'true'
    return result


def initial_actions(c):
    return [action(c, 'hostloss_row1', ledger=artifact(c, 'live-ledger.tsv'),
                   receipt=artifact(c, 'row1.receipt.json'), save_as='row1_receipt'),
            action(c, 'hostloss_lostrange', grant=artifact(c, 'grant.txt'),
                   range=artifact(c, 'range.txt'), request=c['request'],
                   receipt=artifact(c, 'lostrange.receipt.json'), save_as='lostrange_receipt')]


def loss_clock(c):
    loss = read(c, 'loss.receipt.json')
    if (loss.get('clock_domain') != 'host' or loss.get('host') != c['victim']
            or loss.get('boot_id') != c['boot_id'] or not loss.get('unit')
            or not loss.get('invocation')):
        raise ValueError('loss must bind the victim host clock, boot and invocation')
    kill, fence = loss.get('kill_issued_ns'), loss.get('fence_sec')
    if (kill is not None) == (fence is not None):
        raise ValueError('exactly one host kill-issued ns or fence second is required')
    if c['kind'] == 'KB':
        dead = loss.get('confirmed_dead_ns')
        if type(kill) is not int or kill <= 0 or type(dead) is not int or dead < kill:
            raise ValueError('KB needs distinct ordered host kill-issued and confirmed-dead stamps')
        return {'kill_ns': str(kill)}
    if c['kind'] != 'PO' or type(fence) is not int or fence <= 0:
        raise ValueError('PO needs its host fence second')
    return {'fence_sec': str(fence)}


def range_lsn(c):
    r = read(c, 'lostrange.receipt.json')
    if r.get('row') != 'd6-lost-range' or r.get('verdict') != 'PASS' or r.get('counted') is not True:
        raise ValueError('image boundary requires a confirmed lostrange receipt')
    value = r.get('values', {}).get('L', '')
    if not isinstance(value, str) or not re.fullmatch('[0-9]+', value):
        raise ValueError('lostrange receipt has no literal L')
    return value


def followup_actions(c):
    clock = loss_clock(c)
    final = action(c, 'hostloss_final', ledger=artifact(c, 'ledger.tsv'),
                   row1_receipt=artifact(c, 'row1.receipt.json'),
                   receipt=artifact(c, 'final.receipt.json'), save_as='final_receipt', **clock)
    image = action(c, 'hostloss_image', ledger=artifact(c, 'ledger.tsv'),
                   image=artifact(c, 'image.bin'), L=range_lsn(c), blocks=str(c['blocks']),
                   receipt=artifact(c, 'image.receipt.json'), save_as='image_receipt')
    result = [final, image]
    if c['kind'] == 'PO':
        result.append(action(c, 'hostloss_stopbound', ledger=artifact(c, 'ledger.tsv'),
                             journal=artifact(c, 'journal.txt'),
                             receipt=artifact(c, 'stopbound.receipt.json'), save_as='stopbound_receipt'))
    return result


def snapshot_image_action(c, name):
    metadata = read(c, name + '.snapshot.json')
    lsn = metadata.get('cutLSN')
    if type(lsn) is not int or lsn < 0:
        raise ValueError('snapshot image boundary must be its recorded cutLSN')
    return action(c, 'hostloss_image', ledger=artifact(c, 'ledger.tsv'),
                  image=artifact(c, name + '.image.bin'), L=str(lsn), blocks=str(c['blocks']),
                  row='restore-' + name, receipt=artifact(c, name + '.receipt.json'))
