"""CPU test fixture only: HTTP-shaped listeners, no SeaweedFS or RDMA claims."""
import json
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

config = json.loads(sys.argv[1])
role = sys.argv[2]
ports = config['services']

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass
    def do_GET(self):
        if self.path == '/cluster/status':
            body = dict(IsLeader=True)
        elif self.path == '/dir/status':
            body = dict(Topology=dict(DataCenters=[dict(Racks=[dict(DataNodes=[
                dict(Url='127.0.0.1:' + str(ports['volume_http']), Volumes=0)])])]))
        else:
            body = {}
        raw = json.dumps(body).encode()
        self.send_response(200)
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

names = dict(master=('master_http', 'master_grpc'), filer=('filer_http', 'filer_grpc'),
             volume=('volume_http', 'volume_grpc', 'rdma_control', 'dc_control'))[role]
servers = []
for name in names:
    server = ThreadingHTTPServer(('127.0.0.1', ports[name]), Handler)
    servers.append(server)
    threading.Thread(target=server.serve_forever, daemon=True).start()
if role == 'volume':
    print('RDMA listener bound on 127.0.0.1:' + str(ports['rdma']), flush=True)  # CPU fixture, not CM.
    print('RDMA DC push-read writer active', flush=True)  # Deliberate CPU fixture.
print('FIXTURE_LISTENING', flush=True)
while True:
    time.sleep(1)
