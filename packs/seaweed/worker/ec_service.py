"""Exec one pinned EC service in its private owned dir; no child daemon/controller."""
import hashlib,json,os,pathlib,sys
c=json.loads(sys.argv[1]);root=pathlib.Path(c['root'])
s=root.lstat()
if not root.is_absolute() or root.is_symlink() or not root.is_dir() or s.st_uid!=os.getuid() or s.st_mode&0o022:raise ValueError('owned private root required')
folder=root/c['role']
if folder.parent!=root or folder.is_symlink():raise ValueError('foreign service dir')
folder.mkdir(mode=0o700)
exe=pathlib.Path(c['exe'])
if exe.is_symlink() or hashlib.sha256(exe.read_bytes()).hexdigest()!=c['sha256']:raise ValueError('product pin differs')
if c['role'] not in ('master','filer','s3'):
    dirs=[x.split('=',1)[1] for x in c['args'] if x.startswith(('--dir=','-dir='))]
    if dirs!=[str(folder)]:raise ValueError('volume must use only its private dir')
if c['role']=='master' and '-mdir='+str(folder) not in c['args']:raise ValueError('private master state required')
if c['role']=='filer':
    if '-localSocket='+str(root/'filer.sock') not in c['args'] or '-config_dir='+str(root) not in c['args']:raise ValueError('private filer config/socket required')
    config=root/'filer.toml'
    fd=os.open(config,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,'w') as f:f.write('[leveldb2]\nenabled=true\ndir="'+str(folder)+'"\n')
# GO/policy pins the entire argv, including private dirs/ports/localSocket.
os.environ.update(HOME=str(folder),TMPDIR=str(folder));os.chdir(folder)
os.execv(str(exe),[str(exe)]+c['args'])
