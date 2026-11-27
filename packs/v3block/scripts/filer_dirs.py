import json,sys,urllib.request,urllib.parse,urllib.error
d,pre=sys.argv[1],sys.argv[2]; out=[]; last=""
while True:
    q=urllib.parse.urlencode({"limit":1000,"lastFileName":last,"namePattern":pre+"*"})
    try:
        r=json.load(urllib.request.urlopen(urllib.request.Request(d+"/?"+q,headers={"Accept":"application/json"}),timeout=10))
    except urllib.error.HTTPError as e:
        if e.code==404 and not last: break
        raise
    es=r.get("Entries") or []
    out+=[e["FullPath"].rsplit("/",1)[1] for e in es]
    if not r.get("ShouldDisplayLoadMore") or not es: break
    last=es[-1]["FullPath"].rsplit("/",1)[1]
print(json.dumps(sorted(n for n in out if n.startswith(pre))))
