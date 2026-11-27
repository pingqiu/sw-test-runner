import sys;t=sys.argv[1].encode();open(sys.argv[2],"wb").write((t*(4096//len(t)+1))[:4096])
