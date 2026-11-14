#!/usr/bin/env python3
"""Measure create/wait-registration/start/removal using a dedicated engine socket.

Only creates AutoRemove containers with the supplied image. Does not pull images,
change contexts or stop engines. On failure, removes only its own container.
"""
import argparse
import http.client
import json
import socket
import statistics
import time
from pathlib import Path


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__('localhost', timeout=30)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def request(client, method, path, body=None, expected=(200, 201, 204)):
    client.request(method, path, json.dumps(body) if body is not None else None,
                   {'Content-Type': 'application/json'})
    response = client.getresponse()
    data = response.read()
    if response.status not in expected:
        raise RuntimeError(f'{method} {path}: {response.status} {data!r}')
    return json.loads(data) if data else None


def measure(args):
    samples = []
    client = UnixHTTP(args.socket)
    waiter = UnixHTTP(args.socket)
    try:
        for iteration in range(args.runs + 5):
            ident = None
            completed = False
            try:
                t0 = time.perf_counter_ns()
                created = request(client, 'POST', '/containers/create', {
                    'Image': args.image, 'Cmd': ['true'],
                    'HostConfig': {'AutoRemove': True, 'NetworkMode': args.network}})
                ident = created['Id']
                t1 = time.perf_counter_ns()
                waiter.request('POST', f'/containers/{ident}/wait?condition=removed')
                response = waiter.getresponse()
                if response.status != 200:
                    raise RuntimeError(f'wait registration: {response.status}')
                t2 = time.perf_counter_ns()
                request(client, 'POST', f'/containers/{ident}/start', expected=(204,))
                t3 = time.perf_counter_ns()
                result = json.loads(response.read())
                if result.get('StatusCode') != 0 or result.get('Error'):
                    raise RuntimeError(f'container failed: {result}')
                t4 = time.perf_counter_ns()
                completed = True
                if iteration >= 5:
                    samples.append(dict(zip(('create', 'wait_registration', 'start', 'exit_remove', 'total'),
                                            ((t1-t0)/1e6, (t2-t1)/1e6, (t3-t2)/1e6, (t4-t3)/1e6, (t4-t0)/1e6))))
            finally:
                if ident and not completed:
                    client.close()
                    request(client, 'DELETE', f'/containers/{ident}?force=1&v=1', expected=(204, 404))
    finally:
        client.close()
        waiter.close()
    summary = {}
    for key in samples[0]:
        values = sorted(sample[key] for sample in samples)
        summary[key] = {'p50_ms': statistics.median(values),
                        'p99_ms': values[min(int(len(values)*.99), len(values)-1)],
                        'mean_ms': statistics.mean(values)}
    return {'socket': args.socket, 'image': args.image, 'network': args.network,
            'runs': args.runs, 'warmup': 5, 'summary': summary, 'samples_ms': samples}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--socket', required=True)
    parser.add_argument('--image', required=True, help='preloaded immutable image digest')
    parser.add_argument('--network', choices=('none', 'bridge'), default='none')
    parser.add_argument('--runs', type=int, default=100)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--max-p50-ms', type=float, help='optional regression budget for this dedicated environment')
    args = parser.parse_args()
    if args.runs <= 0:
        parser.error('--runs must be positive')
    result = measure(args)
    args.out.write_text(json.dumps(result, indent=2)+'\n')
    print(json.dumps(result['summary'], indent=2))
    if args.max_p50_ms is not None and result['summary']['total']['p50_ms'] > args.max_p50_ms:
        raise SystemExit(f'FAIL: total p50 exceeds {args.max_p50_ms} ms')


if __name__ == '__main__':
    main()
