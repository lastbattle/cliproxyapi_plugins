#!/usr/bin/env python3
"""Opt-in live cancellation check. Uses four backend streams; run only in a quiet window."""

import argparse
import concurrent.futures
import http.client
import json
import os
import time
import urllib.parse
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--backend', default='http://127.0.0.1:8888')
    parser.add_argument('--proxy', default='http://127.0.0.1:8317')
    parser.add_argument('--backend-model', default='GLM-5.3-Flash-EXL3')
    parser.add_argument('--proxy-model', default='glm-5.3-flash-exl3')
    args = parser.parse_args()
    key = os.environ.get('CPA_API_KEY')
    if not key:
        parser.error('Set CPA_API_KEY in the environment; it is never printed')
    backend = args.backend.rstrip('/')
    proxy = urllib.parse.urlsplit(args.proxy)
    if proxy.scheme not in ('http', 'https') or not proxy.hostname or proxy.query or proxy.fragment or proxy.username:
        parser.error('--proxy must be an http(s) base URL without credentials, query or fragment')

    def health():
        with urllib.request.urlopen(backend + '/health', timeout=5) as response:
            return json.load(response)

    def wait_for(predicate, timeout):
        deadline = time.monotonic() + timeout
        while True:
            state = health()
            if predicate(state) or time.monotonic() >= deadline:
                return state
            time.sleep(0.1)

    def active(number):
        body = {'model': args.backend_model, 'messages': [{'role': 'user', 'content':
                f'Connection check {number}. List integers continuously.'}],
                'stream': True, 'max_tokens': 32768, 'ignore_eos': True}
        return urllib.request.urlopen(urllib.request.Request(backend + '/v1/chat/completions',
                                      json.dumps(body).encode(), {'Content-Type': 'application/json'}), timeout=30)

    if health()['requests_running']:
        print('SKIP: existing user traffic; no synthetic requests started')
        return 3
    streams = []
    try:
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            futures = [pool.submit(active, number) for number in range(4)]
            errors = []
            for future in concurrent.futures.as_completed(futures):
                try:
                    streams.append(future.result())
                except Exception as exc:
                    errors.append(exc)
            if errors:
                raise RuntimeError('Could not open four backend streams') from errors[0]
        state = wait_for(lambda h: h['streams']['decoding'] == 4, 15)
        if state['streams']['decoding'] != 4:
            raise RuntimeError('Four decoding lanes did not become active')
        for path, streaming in (('/v1/chat/completions', False), ('/v1/chat/completions', True), ('/v1/responses', True)):
            before = health()['multi_prefill'].copy()
            text = 'Cancellation verification: reply exactly OK'
            if path.endswith('responses'):
                body = {'model': args.proxy_model, 'input': text, 'max_output_tokens': 8, 'stream': True}
            else:
                body = {'model': args.proxy_model, 'messages': [{'role': 'user', 'content': text}],
                        'max_tokens': 8, 'stream': streaming}
            connection_class = http.client.HTTPSConnection if proxy.scheme == 'https' else http.client.HTTPConnection
            connection = connection_class(proxy.hostname, proxy.port, timeout=10)
            try:
                connection.request('POST', proxy.path.rstrip('/') + path, json.dumps(body),
                                   {'Content-Type': 'application/json', 'Authorization': 'Bearer ' + key})
                state = wait_for(lambda h: h['requests_running'] == 5, 12)
                if state['requests_running'] != 5:
                    raise RuntimeError(f'{path} never appeared in the backend queue; check route/auth/load')
            finally:
                connection.close()
            start = time.monotonic()
            state = wait_for(lambda h: h['requests_running'] == 4, 12)
            if state['requests_running'] != 4 or state['streams']['decoding'] != 4:
                raise RuntimeError(f'{path} cancellation did not remove queued work with four lanes still active')
            if state['multi_prefill'] != before:
                raise RuntimeError('Additional GPU prefill chunks ran; concurrent user traffic can invalidate this check')
            print(f'PASS {path} stream={streaming}: upstream removed in {time.monotonic()-start:.2f}s; no added GPU prefill', flush=True)
    finally:
        for response in streams:
            response.close()
        state = wait_for(lambda h: h['requests_running'] == 0, 30)
        print(f"Remaining backend requests after cleanup: {state['requests_running']}", flush=True)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
