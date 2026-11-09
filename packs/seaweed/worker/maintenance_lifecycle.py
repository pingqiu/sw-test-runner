"""Exact-run lifecycle leaf for the maintenance selector, not a general installer."""
import json
import os
import pathlib
import re
import stat
import subprocess
import sys
import time


def command(argv, timeout=10):
    r=subprocess.run(argv,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=timeout)
    if r.returncode or len(r.stdout)>2*1024*1024 or len(r.stderr)>65536:
        raise RuntimeError('lifecycle command refused: '+argv[0]+' exit='+str(r.returncode))
    return r.stdout.decode('utf8','strict')


def receipt(path, run, role, uid):
    st=os.stat(path,follow_symlinks=False)
    if not stat.S_ISDIR(st.st_mode) or st.st_uid!=uid or pathlib.Path(path).resolve()!=pathlib.Path(path):
        raise ValueError('root identity/owner/path differs')
    return dict(schema='seaweed.maintenance-root/v1',run_id=run,role_run_id=role,
                path=path,dev=st.st_dev,ino=st.st_ino,uid=uid)


def prepare(c):
    value=receipt(c['root'],c['run_id'],c['role_run_id'],os.getuid())
    path=pathlib.Path(c['root'])/'.maintenance-owner.json'
    fd=os.open(path,os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'w') as f:
        json.dump(value,f,sort_keys=True);f.flush();os.fsync(f.fileno())
    return value


def unit_state(name):
    text=command(['systemctl','show',name,'--no-pager','--property=LoadState,MainPID,WorkingDirectory,FragmentPath,ActiveState'])
    values={}
    for line in text.splitlines():
        k,sep,v=line.partition('=')
        if not sep or k in values:raise ValueError('unit response malformed')
        values[k]=v
    if set(values)!={'LoadState','MainPID','WorkingDirectory','FragmentPath','ActiveState'}:
        raise ValueError('unit response incomplete')
    return values


def owned_units(c):
    ready=[]
    for item in c['units']:
        unit,root=item['unit'],item['root']
        if not re.fullmatch(r'sw-cloud-[a-zA-Z0-9-]+-(?:master|filer|s3|volume|maintenance-task|maintenance-admin|maintenance-worker)(?:-g[2-4])?\.service',unit):
            raise ValueError('unadmitted exact unit')
        state=unit_state(unit)
        if state['LoadState']=='not-found':continue
        if state['WorkingDirectory']!=root or state['FragmentPath']!='/run/systemd/transient/'+unit:
            raise ValueError('foreign unit binding: '+unit)
        ready.append(unit)
    return ready


def process_absent(roots):
    for p in pathlib.Path('/proc').iterdir():
        if not p.name.isdecimal():continue
        for name in ('cwd','exe'):
            try:value=os.readlink(p/name)
            except (FileNotFoundError,ProcessLookupError):continue
            # A permission failure is UNKNOWN, not proof of absence.
            if any(value==root or value.startswith(root+'/') for root in roots):
                raise ValueError('run process remains: '+p.name)
        try:
            args=(p/'cmdline').read_bytes().split(b'\0')
            mappings=(p/'maps').read_text().splitlines()
        except (FileNotFoundError,ProcessLookupError):continue
        values=[a.decode('utf8','replace') for a in args]
        values.extend(parts[5] for line in mappings if len(parts:=line.split(None,5))==6)
        if any(value==root or value.startswith(root+'/') for value in values for root in roots):
            raise ValueError('run helper or mapped artifact remains: '+p.name)


def prevalidate_root(expected,tree=True):
    root=expected['path']
    if receipt(root,expected['run_id'],expected['role_run_id'],expected['uid'])!=expected:
        raise ValueError('replacement root')
    fd=os.open(root,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
    try:
        opened=os.fstat(fd)
        if opened.st_dev!=expected['dev'] or opened.st_ino!=expected['ino'] or opened.st_uid!=expected['uid']:
            raise ValueError('root changed between receipt and open')
        mark=os.open('.maintenance-owner.json',os.O_RDONLY|os.O_NOFOLLOW,dir_fd=fd)
        with os.fdopen(mark,'rb') as f:
            st=os.fstat(f.fileno());raw=f.read(65537)
            if not stat.S_ISREG(st.st_mode) or st.st_uid!=expected['uid'] or st.st_nlink!=1 or stat.S_IMODE(st.st_mode)!=0o600 or len(raw)>65536:
                raise ValueError('foreign root receipt')
        def pairs(items):
            d={}
            for k,v in items:
                if k in d:raise ValueError('duplicate receipt key')
                d[k]=v
            return d
        if json.loads(raw,object_pairs_hook=pairs)!=expected:raise ValueError('root receipt differs')
        if tree:validate_tree(fd,expected['uid'],expected['dev'])
        return fd
    except BaseException:
        os.close(fd);raise


def validate_tree(fd,uid,dev):
    for name in os.listdir(fd):
        st=os.stat(name,dir_fd=fd,follow_symlinks=False)
        if st.st_dev!=dev or st.st_uid not in (0,uid) or stat.S_ISLNK(st.st_mode):
            raise ValueError('foreign owner/symlink/mount in run root')
        if stat.S_ISDIR(st.st_mode):
            child=os.open(name,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
            try:validate_tree(child,uid,dev)
            finally:os.close(child)
        elif not (stat.S_ISREG(st.st_mode) or stat.S_ISSOCK(st.st_mode)) or st.st_nlink!=1:
            raise ValueError('foreign nonexclusive file in run root')


def delete_tree(fd):
    for name in os.listdir(fd):
        st=os.stat(name,dir_fd=fd,follow_symlinks=False)
        if stat.S_ISDIR(st.st_mode):
            child=os.open(name,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
            try:delete_tree(child)
            finally:os.close(child)
            os.rmdir(name,dir_fd=fd)
        else:
            # Revalidate again at mutation; never follow a replacement link.
            if not (stat.S_ISREG(st.st_mode) or stat.S_ISSOCK(st.st_mode)) or st.st_nlink!=1:raise ValueError('cleanup file changed')
            os.unlink(name,dir_fd=fd)


def cleanup(c):
    units=owned_units(c)
    if units:
        command(['systemctl','stop',*units],35)
        failed=[name for name in units if unit_state(name)['ActiveState']=='failed']
        if failed:command(['systemctl','reset-failed',*failed],10)
    until=time.monotonic()+5
    while True:
        remaining=[]
        for item in c['units']:
            s=unit_state(item['unit'])
            if s['MainPID']!='0' or s['ActiveState']!='inactive' or s['LoadState']!='not-found':remaining.append(item['unit'])
        if not remaining:break
        if time.monotonic()>=until:raise ValueError('run units not gone: '+','.join(remaining))
        time.sleep(.05)
    ports=set(c['ports'])
    for line in command(['ss','-Hltn']).splitlines():
        f=line.split()
        if len(f)<4:raise ValueError('socket response malformed')
        if int(f[3].rsplit(':',1)[1]) in ports:raise ValueError('run listener remains')
    unix=command(['ss','-Hlxnp'])
    if any(root['path']+'/' in unix for root in c['roots']):raise ValueError('run Unix listener remains')
    process_absent([x['path'] for x in c['roots']])
    if not c.get('remove',False):
        return dict(status='STOPPED_EVIDENCE_RETAINED',removed=[],units=0,listeners=0,run_processes=0)
    handles=[]
    try:
        # Validate ALL roots before the first deletion. An unacknowledged
        # prepare leaves a root without a controller receipt: refuse it.
        expected={x['path']:x for x in c['receipts']}
        for item in c['roots']:
            path=item['path']
            if not os.path.lexists(path):
                if path in expected:raise ValueError('recorded root disappeared')
                continue
            if path not in expected:raise ValueError('unconfirmed prepare root retained')
            handles.append((path,prevalidate_root(expected[path])))
        removed=[]
        for path,fd in handles:
            if os.stat(path,follow_symlinks=False).st_ino!=os.fstat(fd).st_ino:raise ValueError('root replaced before removal')
            delete_tree(fd);os.rmdir(path);removed.append(path)
        for item in c['roots']:
            if os.path.lexists(item['path']):raise ValueError('root remains after cleanup')
        process_absent([x['path'] for x in c['roots']])
        return dict(status='CLEAN',removed=removed,units=0,listeners=0,run_processes=0)
    finally:
        for _,fd in handles:os.close(fd)


def preserve(c):
    owned_units(c)  # never collect a replacement foreign unit's journal
    out={};total=0;records={x['path']:x for x in c['receipts']}
    for item in c['roots']:
        root=pathlib.Path(item['path']);files={}
        if not os.path.lexists(root):out[str(root)]={};continue
        if str(root) not in records:out[str(root)]=dict(unconfirmed_prepare=True);continue
        rootfd=prevalidate_root(records[str(root)],tree=False)
        names=['.maintenance-owner.json','maintenance/config.json','maintenance/manifest.json',
               'maintenance/acks.jsonl','maintenance/result.json','maintenance/events.jsonl','maintenance/task.log','maintenance/lock-check.json']
        try:
            try:volume=os.open('volume',os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=rootfd)
            except FileNotFoundError:volume=None
            if volume is not None:
                try:names.extend('volume/'+name for name in os.listdir(volume) if name.startswith(c['collection']+'_') and pathlib.Path(name).suffix in ('.active','.prepared','.last_committed','.vif'))
                finally:os.close(volume)
            for name in names:
                parent=os.dup(rootfd)
                try:
                    components=name.split('/')
                    for component in components[:-1]:
                        nextfd=os.open(component,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=parent);os.close(parent);parent=nextfd
                    fd=os.open(components[-1],os.O_RDONLY|os.O_NOFOLLOW,dir_fd=parent)
                except FileNotFoundError:continue
                finally:os.close(parent)
                with os.fdopen(fd,'rb') as f:
                    st=os.fstat(f.fileno())
                    if not stat.S_ISREG(st.st_mode) or st.st_nlink!=1 or st.st_uid not in (0,records[str(root)]['uid']):raise ValueError('foreign evidence file')
                    raw=f.read(65537);total+=len(raw)
                    if total>1024*1024:raise ValueError('maintenance evidence cap')
                    files[name]=dict(text=raw[:65536].decode('utf8','replace'),truncated=len(raw)>65536)
        finally:os.close(rootfd)
        out[str(root)]=files
    units=[x['unit'] for x in c['units']]
    journal=command(['journalctl','--no-pager','-n','100',*sum((['-u',x] for x in units),[])],20)
    if len(journal.encode())>1024*1024:raise ValueError('maintenance journal cap')
    return dict(roots=out,journal=journal)


def fresh(c):
    for item in c['units']:
        if unit_state(item['unit'])['LoadState']!='not-found':raise ValueError('run unit already exists')
    for item in c['roots']:
        if os.path.lexists(item['path']):raise ValueError('run root already exists')
    ports=set(c['ports'])
    for line in command(['ss','-Hltn']).splitlines():
        f=line.split()
        if len(f)<4:raise ValueError('socket response malformed')
        if int(f[3].rsplit(':',1)[1]) in ports:raise ValueError('declared port already in use')
    return dict(fresh=True)


if __name__=='__main__':
    c=json.loads(sys.argv[1])
    result={'prepare':prepare,'cleanup':cleanup,'preserve':preserve,'fresh':fresh}[c['stage']](c)
    print(json.dumps(result,allow_nan=False,sort_keys=True),flush=True)
