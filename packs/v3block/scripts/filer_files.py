import json,sys,urllib.request,urllib.parse,urllib.error
root=sys.argv[1]; out=[]
def ls(d,rel):
    last=""
    while True:
        q=urllib.parse.urlencode({"limit":1000,"lastFileName":last})
        try:
            r=json.load(urllib.request.urlopen(urllib.request.Request(d+"/?"+q,headers={"Accept":"application/json"}),timeout=10))
        except urllib.error.HTTPError as e:
            if e.code==404 and not last: return
            raise
        es=r.get("Entries") or []
        for e in es:
            name=e["FullPath"].rsplit("/",1)[1]
            if e.get("Mode",0)&0o20000000000: ls(d+"/"+urllib.parse.quote(name),rel+name+"/")
            else: out.append(rel+name)
        if not r.get("ShouldDisplayLoadMore") or not es: return
        last=es[-1]["FullPath"].rsplit("/",1)[1]
ls(root,"")
print(json.dumps(sorted(out)))
