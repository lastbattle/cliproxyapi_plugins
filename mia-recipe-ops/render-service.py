#!/usr/bin/env python3
"""Stage a system service for this machine; does not install or start it."""

import argparse
import getpass
from pathlib import Path
import re
import shutil


def env_quote(value):
    return '"' + str(value).replace('\\', '\\\\').replace('"', '\\"') + '"'


def unit_quote(value):
    return env_quote(str(value).replace('%', '%%'))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--recipe-dir', type=Path, required=True)
    parser.add_argument('--worker', required=True, help='Key-authenticated SSH target, e.g. user@192.0.2.2')
    parser.add_argument('--output-dir', type=Path, required=True, help='Permanent runtime directory owned by --user')
    parser.add_argument('--user', default=getpass.getuser())
    parser.add_argument('--service', default='mia-recipe.service')
    parser.add_argument('--api-base', default='http://127.0.0.1:8888')
    parser.add_argument('--model', default='GLM-5.3-Flash-EXL3')
    parser.add_argument('--container', default='glm53-flash-tf')
    args = parser.parse_args()
    values = [str(value) for value in vars(args).values()]
    if any(any(c in value for c in '\n\r\x00') for value in values):
        parser.error('Arguments must be single-line values without NUL bytes')
    if not re.fullmatch(r'[a-z_][a-z0-9_-]*\$?', args.user):
        parser.error('--user must be a Linux account name')
    if not re.fullmatch(r'[a-zA-Z0-9_-]+\.service', args.service):
        parser.error('--service must be a simple .service unit name')
    if not args.worker or args.worker.startswith('-'):
        parser.error('--worker must be an SSH destination, not an option')
    recipe = args.recipe_dir.expanduser().resolve()
    for name in ('start.sh', 'stop.sh'):
        if not (recipe / name).is_file():
            parser.error(f'Missing recipe entry point: {recipe / name}')
    source = Path(__file__).resolve().parent
    output = args.output_dir.expanduser().resolve()
    names = ('supervise.sh', 'watchdog.py', 'runtime.env', args.service)
    if output == source:
        parser.error('Use a separate runtime directory, outside this source checkout')
    if any((output / name).exists() for name in names):
        parser.error('Output files already exist; back up or choose a new runtime directory')
    output.mkdir(parents=True, exist_ok=True, mode=0o700)
    for name in ('supervise.sh', 'watchdog.py'):
        shutil.copyfile(source / name, output / name)
        (output / name).chmod(0o700)
    settings = {
        'MIA_RECIPE_DIR': str(recipe), 'MIA_WORKER': args.worker,
        'MIA_API_BASE': args.api_base, 'MIA_MODEL': args.model,
        'MIA_CONTAINER': args.container, 'MIA_SERVICE': args.service,
        'MIA_STATE_DIR': str(output / 'state'),
        'MIA_PROGRESS_CHECK': '1', 'MIA_STACK_SIGNAL': '0',
        'MIA_POLL_S': '20', 'MIA_DECODE_STALL_S': '240',
        'MIA_PREFILL_STALL_S': '1800', 'MIA_ITERATION_STALL_S': '210',
        'MIA_LIVE_METRIC_PREFIX': 'tensorfold:kv_cache_usage_ratio',
    }
    env = output / 'runtime.env'
    env.write_text(''.join(f'{key}={env_quote(value)}\n' for key, value in settings.items()))
    env.chmod(0o600)
    unit = f'''[Unit]
Description=Mia TensorFold supervised two-DGX-Spark cluster
Wants=network-online.target
After=network-online.target docker.service
Requires=docker.service
StartLimitIntervalSec=1800
StartLimitBurst=3

[Service]
Type=simple
User={args.user}
UMask=0077
EnvironmentFile={unit_quote(env)}
WorkingDirectory={unit_quote(recipe)}
ExecStart=/bin/bash {unit_quote(output / 'supervise.sh')}
ExecStop=/bin/bash {unit_quote(recipe / 'stop.sh')}
Restart=on-failure
RestartSec=60
TimeoutStopSec=120

[Install]
WantedBy=multi-user.target
'''
    (output / args.service).write_text(unit)
    (output / args.service).chmod(0o600)
    print(f'Staged {output / args.service}; no service installed, no recipe changed, no restart performed.')


if __name__ == '__main__':
    main()
