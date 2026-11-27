import os,sys;fd=os.open(sys.argv[1],os.O_WRONLY);os.fsync(fd);os.close(fd)
