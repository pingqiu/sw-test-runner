"""Read-only live VF binding; no network/device configuration changes."""
import ipaddress,json,re,subprocess,sys
from pathlib import Path

def run(argv):
    p=subprocess.run(['timeout','-k','2','10',*argv],capture_output=True,text=True,timeout=13)
    if p.returncode:raise RuntimeError('observation refused: '+repr(argv)+': '+p.stderr)
    return p.stdout

def bound_gids(port,expected_ip):
    # Empty/unrelated GID slots may have unreadable ndevs attributes. Only
    # candidate entries for this VF address require type/netdev observations.
    gids=[]
    for p in sorted((port/'gids').iterdir(),key=lambda p:int(p.name)):
        value=p.read_text().strip()
        if str(ipaddress.ip_address(value).ipv4_mapped)!=expected_ip:continue
        gids.append(dict(index=int(p.name),gid=value,
            type=(port/'gid_attrs/types'/p.name).read_text().strip(),
            netdev=(port/'gid_attrs/ndevs'/p.name).read_text().strip()))
    return gids

def cap_mbps(row):
    # iproute2 JSON uses max_tx; max_tx_rate is a text-output label.
    rate=row.get('rate',{}).get('max_tx')
    cross=row.get('tx_rate')
    if rate!=40000 or cross!=rate:
        raise RuntimeError('expected existing 40000 Mbit/s VF cap, observed '+repr(rate)+
                           '; tx_rate cross-check '+repr(cross))
    return rate

def observe(host):
    result=dict(host=host,rdma_devices={},addresses=[],links=[],devices_raw={})
    try:
        result['links']=json.loads(run(['ip','-j','-d','link']))
        result['addresses']=json.loads(run(['ip','-j','address']))
        if host not in ('m01','m02'):raise ValueError('host binding')
        last='1' if host=='m01' else '3'
        for rail in (0,1):
            name='mlx5_'+str(rail);device=Path('/sys/class/infiniband')/name;port=device/'ports/1'
            state=(port/'state').read_text().strip()
            if 'ACTIVE' not in state.upper():raise RuntimeError(name+' not ACTIVE')
            ip=f'10.0.{rail+2}.{last}'
            gids=bound_gids(port,ip)
            matches=[g for g in gids if g['type']=='RoCE v2' and
                     str(ipaddress.ip_address(g['gid']).ipv4_mapped)==ip]
            if len(matches)!=1 or not matches[0]['netdev']:raise RuntimeError('ambiguous VF/GID '+name)
            actual=[r for r in result['addresses'] if r['ifname']==matches[0]['netdev'] and
                    any(a.get('local')==ip for a in r.get('addr_info',[]))]
            if len(actual)!=1:raise RuntimeError('VF address/netdev binding missing')
            raw=run(['ibv_devinfo','-d',name,'-i','1']);result['devices_raw'][name]=raw
            mtu=re.search(r'active_mtu:\s*(\d+)',raw)
            if not mtu or int(mtu.group(1))<4096:raise RuntimeError('VF MTU not sufficient')
            physical=(device/'device/physfn').resolve(strict=True)
            slots=[v for v in physical.glob('virtfn*') if v.resolve()==(device/'device').resolve()]
            if len(slots)!=1:raise RuntimeError('VF function index ambiguous')
            vf=int(slots[0].name.removeprefix('virtfn'))
            pf_names=[p.name for p in (physical/'net').iterdir()]
            vf_rows=[v for link in result['links'] if link['ifname'] in pf_names
                     for v in link.get('vfinfo_list',[]) if v.get('vf')==vf]
            if len(vf_rows)!=1:raise RuntimeError('VF cap observation absent or ambiguous')
            rate=cap_mbps(vf_rows[0])
            result['rdma_devices'][name]=dict(pci=str((device/'device').resolve()),gids=gids,
                chosen=matches[0],active_mtu=int(mtu.group(1)),state=state,
                physical_pci=str(physical),vf_index=vf,max_tx_rate_mbps=rate)
        if result['rdma_devices']['mlx5_0']['pci']==result['rdma_devices']['mlx5_1']['pci']:
            raise RuntimeError('VF PCI alias')
        return result
    except Exception as error:
        # Keep stdout a single JSON on success; every refusal retains raw VF JSON.
        print(json.dumps(dict(status='REFUSED',observation=result,
                              raw_vfinfo_json=result['links'],error=repr(error))),
              file=sys.stderr,flush=True)
        raise

if __name__=='__main__':
    print(json.dumps(observe(sys.argv[1]),indent=2))
