"""Frozen owned EC files/holders, first encode only (no vacuum generation support)."""
import hashlib,json,os,pathlib,re,stat,sys

def stable(path,hash_body=True):
    a=path.lstat()
    if not stat.S_ISREG(a.st_mode) or a.st_uid!=os.getuid():raise ValueError('foreign/nonregular EC file')
    h=hashlib.sha256()
    with path.open('rb') as f:
        if hash_body:
            for block in iter(lambda:f.read(1048576),b''):h.update(block)
    b=path.stat()
    if (a.st_dev,a.st_ino,a.st_size,a.st_mtime_ns,a.st_ctime_ns)!=(b.st_dev,b.st_ino,b.st_size,b.st_mtime_ns,b.st_ctime_ns):raise ValueError('EC file changed during capture')
    return dict(sha256=h.hexdigest(),bytes=a.st_size)

def capture(c,manifest):
    root=pathlib.Path(c['root']);rootstat=root.lstat()
    if not root.is_absolute() or not stat.S_ISDIR(rootstat.st_mode) or rootstat.st_uid!=os.getuid() or rootstat.st_mode&0o022:raise ValueError('private owned root required')
    if not 1<=len(c['holders'])<=14 or manifest['run_id']!=c['run_id']:raise ValueError('holder/manifest scope')
    boot=pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip();holders=[];configs={};indexes={};shards=[];files=[]
    for label,folder in c['holders'].items():
        directory=pathlib.Path(folder)
        if directory.parent!=root or directory.is_symlink() or not directory.is_dir() or directory.stat().st_uid!=os.getuid() or directory.stat().st_mode&0o022:raise ValueError('foreign holder dir')
        if any(directory.glob('*.dat*')) or any(directory.glob('*.active')):raise ValueError('original/unsupported active generation present')
        matches=[]
        for p in pathlib.Path('/proc').glob('[0-9]*'):
            try:
                args=p.joinpath('cmdline').read_bytes().split(b'\0');args=[v.decode() for v in args if v]
                if not args or args[0]!=c['volume_exe'] or not ('--dir='+str(directory) in args or '-dir='+str(directory) in args or any(v in ('--dir','-dir') and i+1<len(args) and args[i+1]==str(directory) for i,v in enumerate(args))):continue
                fields=p.joinpath('stat').read_text().rsplit(') ',1)[1].split();exe=p.joinpath('exe').resolve();digest=hashlib.sha256(exe.read_bytes()).hexdigest()
                if digest!=c['volume_sha256']:raise ValueError('holder product hash mismatch')
                matches.append(dict(job=label,pid=int(p.name),starttime=int(fields[19]),boot_id=boot))
            except (FileNotFoundError,ProcessLookupError,PermissionError):continue
        if len(matches)!=1:raise ValueError('holder identity absent/ambiguous')
        holders.append(matches[0])
        members=[]
        for path in directory.iterdir():
            if path.suffix=='.vif':
                vif_bytes=path.read_bytes();proof=stable(path)
                if proof['sha256']!=hashlib.sha256(vif_bytes).hexdigest():raise ValueError('vif parse/hash changed')
                raw=json.loads(vif_bytes);cfg=raw.get('ecShardConfig',{});generation=int(cfg.get('encodeTsNs',0))
                if int(cfg.get('dataShards',0))!=10 or int(cfg.get('parityShards',0))!=4 or generation<=0:raise ValueError('requires explicit10+4 encode generation')
                vid=path.stem.rsplit('_',1)[-1]
                if vid in configs and configs[vid]!=generation:raise ValueError('mixed encode generations')
                configs[vid]=generation
            match=re.fullmatch(r'.*?(\d+)\.ec(\d\d)',path.name)
            if match:
                members.append(path)
                vid,number=match.group(1),int(match.group(2))
                if number>=14:raise ValueError('foreign shard id')
                shards.append(dict(volume=vid,shard=number,job=label,**stable(path)))
            elif path.suffix in ('.vif','.ecx'):files.append(dict(job=label,name=path.name,**stable(path)))
        if len(members)!=1:raise ValueError('one shard per holder required')
        member=members[0];vid=re.fullmatch(r'.*?(\d+)\.ec(\d\d)',member.name).group(1)
        if not member.with_suffix('.vif').is_file() or not member.with_suffix('.ecx').is_file():raise ValueError('holder-local metadata/index missing')
        index=stable(member.with_suffix('.ecx'))['sha256']
        if vid in indexes and indexes[vid]!=index:raise ValueError('foreign holder index')
        indexes[vid]=index
        shards[-1].update(generation=configs[vid],index_sha256=index,vif_sha256=stable(member.with_suffix('.vif'))['sha256'])
    volumes={o['fid'].split(',')[0] for o in manifest['objects']}
    if set(configs)!=volumes or {s['volume'] for s in shards}!=volumes:raise ValueError('manifest volume set differs')
    for vid in volumes:
        if {s['shard'] for s in shards if s['volume']==vid}!=set(range(14)):raise ValueError('missing shard')
    return dict(manifest=dict(manifest,generation=json.dumps(configs,sort_keys=True)),holders=holders,shards=shards,files=files,original_absent=True,exposure='NOT_OBSERVED')

if __name__=='__main__':
    c=json.loads(sys.argv[1]);m=json.loads(pathlib.Path(sys.argv[2]).read_bytes())
    print(json.dumps(capture(c,m),allow_nan=False),flush=True)
