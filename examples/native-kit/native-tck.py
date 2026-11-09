#!/usr/bin/env python3
"""Candidate native Embed TCK adapter. Run with an explicit fixture catalog/claims.

All guest observations use envd, and all boot/stop/remove operations use the
public API. Storage observations read the production host registry and backing
directories. A missing fixture, transport failure or failed build is an error.
Only the worker admission selector's explicit refusal exits with status 2.
"""
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid


class Refused(Exception):
    pass


def helper(*args, data=None):
    p = subprocess.run([os.environ['NATIVE_TCK_HELPER'], *args], input=data,
                       text=True, capture_output=True, timeout=600)
    if p.returncode == 2:
        raise Refused(p.stderr.strip())
    if p.returncode:
        raise RuntimeError(p.stderr.strip() or 'native TCK helper failed')
    return p.stdout


def api(method, path, body=None):
    key = pathlib.Path(os.environ['NATIVE_TCK_API_KEY_FILE']).read_text().strip()
    req = urllib.request.Request(os.environ.get('E2B_API_URL', 'http://127.0.0.1:3000') + path,
        data=None if body is None else json.dumps(body).encode(), method=method,
        headers={'X-API-Key': key, 'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(req, timeout=180) as response:
            raw = response.read()
            return json.loads(raw) if raw else None
    except urllib.error.HTTPError as exc:
        # HTTP 500 and arbitrary errors are never classified as policy refusal.
        with exc:
            detail = exc.read().decode()
        raise RuntimeError(f'{method} {path}: HTTP {exc.code}: {detail}') from exc


def save(path, data):
    temporary = path.with_name(path.name + '.' + uuid.uuid4().hex)
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as stream:
        json.dump(data, stream)
    os.replace(temporary, path)


def options(args):
    refs, values = [], {'args': {}, 'env': {}, 'reject': []}
    it = iter(args)
    for arg in it:
        if not arg.startswith('--'):
            refs.append(arg)
            continue
        value = next(it, None)
        if value is None:
            raise ValueError('missing option value')
        if arg in ('--arg', '--env'):
            k, separator, v = value.partition('=')
            if not k or not separator:
                raise ValueError('expected name=value')
            values['args' if arg == '--arg' else 'env'][k] = v
        elif arg == '--reject-capability':
            values['reject'].append(value)
        elif arg == '--name':
            if not re.fullmatch(r'[A-Za-z0-9_-]{1,128}', value):
                raise ValueError('invalid instance alias')
            values['name'] = value
        elif arg == '--skills-host-mode' and value == 'off':
            pass  # Native backend's ordinary host sharing setting is off.
        else:
            raise RuntimeError('native TCK input is not wired: ' + arg)
    return refs, values


def build_template(root, source, plan):
    cache = root / ('template-' + hashlib.sha256(json.dumps(source, sort_keys=True).encode()).hexdigest() + '.json')
    if cache.exists():
        template = json.loads(cache.read_text())
    else:
        resources = plan['resources']
        allocation = api('POST', '/v3/templates', {
            'name': 'native-tck-' + uuid.uuid4().hex[:12],
            'cpuCount': max(2, resources['cpus']),
            'memoryMB': max(512, resources['memoryMiB'])})
        tid, bid = allocation['templateID'], allocation['buildID']
        api('POST', f'/v2/templates/{tid}/builds/{bid}', {'fromKit': source})
        deadline = time.monotonic() + 600
        while True:
            status = api('GET', f'/templates/{tid}/builds/{bid}/status')
            if status['status'] == 'ready':
                break
            if status['status'] == 'error':
                raise RuntimeError('native template build failed: ' + json.dumps(status))
            if time.monotonic() > deadline:
                raise TimeoutError('native template build exceeded ten minutes')
            time.sleep(1)
        template = {'templateID': tid, 'buildID': bid}
        save(cache, template)
    return template


def main(argv):
    if not argv:
        raise ValueError('adapter verb required')
    known = json.loads(helper('types'))
    claims = [x for x in os.environ.get('NATIVE_TCK_CLAIMS', '').split(',') if x]
    if not claims or any(x not in known for x in claims):
        raise ValueError('NATIVE_TCK_CLAIMS must explicitly select known candidate types')
    verb = argv[0]
    if verb == 'capabilities':
        print('\n'.join(claims))
        return 0
    root = pathlib.Path(os.environ['NATIVE_TCK_STATE'])
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    if root.is_symlink() or not root.is_dir() or root.stat().st_mode & 0o077:
        raise ValueError('native TCK state must be a private directory, not a symlink')
    if verb in ('host-mounts', 'host-mount-read', 'host-mount-rm'):
        expected = 3 if verb == 'host-mount-read' else 2
        if len(argv) != expected:
            raise ValueError('invalid host storage operation arguments')
        if verb == 'host-mounts':
            catalog = json.loads(pathlib.Path(os.environ['NATIVE_TCK_CATALOG']).read_text())
            if argv[1] not in catalog.values():
                raise ValueError('fixture reference is absent from native TCK catalog')
        # Observe and administer the real tenant-scoped registry. These verbs
        # have no instance alias and must work after sandbox deletion.
        sys.stdout.write(helper(verb, os.environ['NATIVE_TCK_TEAM_ID'], *argv[1:]))
        return 0
    if verb == 'create':
        refs, flags = options(argv[1:])
        catalog = json.loads(pathlib.Path(os.environ['NATIVE_TCK_CATALOG']).read_text())
        if not refs or any(ref not in catalog.values() for ref in refs):
            raise RuntimeError('fixture reference is absent from native TCK catalog')
        source = {'references': refs, 'args': flags['args'], 'env': flags['env'],
                  'options': {'rejectCapabilities': sorted(set(known) - set(claims) | set(flags['reject']))}}
        # Admission uses the production worker compiler. It determines the
        # identity, final env and selection; the adapter supplies no outcomes.
        plan = json.loads(helper('plan', data=json.dumps(source)))
        template = build_template(root, source, plan)
        identity = flags.get('name', 'instance-' + uuid.uuid4().hex)
        path = root / (identity + '.json')
        if path.exists():
            raise RuntimeError('instance alias already exists')
        state = api('POST', '/sandboxes', {'templateID': template['templateID'], 'timeout': 900})
        state.update(user=plan['identity']['name'], cwd=plan['cwd'], env=plan['env'], selection=plan['selection'], stopped=False, source=source, futureReject=flags['reject'])
        try:
            save(path, state)
        except Exception:
            api('DELETE', '/sandboxes/' + state['sandboxID'])
            raise
        print(identity)
        return 0
    if len(argv) < 2 or not re.fullmatch(r'[A-Za-z0-9_-]{1,128}', argv[1]):
        raise ValueError('valid instance ID required')
    path = root / (argv[1] + '.json')
    if verb == 'volume-paths':
        observed = path if path.exists() else root / (argv[1] + '.removed.json')
        state = json.loads(observed.read_text())
        print(helper('volume-paths', os.environ['NATIVE_TCK_TEAM_ID'], state['sandboxID']).strip())
        return 0
    state = json.loads(path.read_text())
    sid = state['sandboxID']
    if verb == 'selection-policy':
        refs, flags = options(argv[2:])
        if refs or flags['args'] or flags['env'] or 'name' in flags:
            raise ValueError('selection-policy accepts only capability rejections')
        if any(x not in known for x in flags['reject']):
            raise ValueError('unknown capability rejection')
        # Changes only the next template selection. Current grants and the
        # paused snapshot remain immutable through an ordinary restart.
        state['futureReject'] = flags['reject']
        save(path, state)
        return 0
    if verb == 'recreate':
        refs, flags = options(argv[2:])
        source = json.loads(json.dumps(state['source']))
        if refs:
            catalog = json.loads(pathlib.Path(os.environ['NATIVE_TCK_CATALOG']).read_text())
            if any(ref not in catalog.values() for ref in refs):
                raise RuntimeError('fixture reference is absent from native TCK catalog')
            source['references'] = refs
            # Kit arguments belong to this composition. Removed Kits must not
            # leave unknown arguments in an explicit replacement request.
            source['args'] = {}
        if 'futureReject' in state:
            source['options']['rejectCapabilities'] = sorted(
                set(known) - set(claims) | set(state['futureReject']))
        source['args'].update(flags['args'])
        source['env'].update(flags['env'])
        if flags['reject']:
            raise ValueError('recreate does not accept rejection changes')
        plan = json.loads(helper('plan', data=json.dumps(source)))
        # Production compatibility check, before pausing the running instance.
        # The worker repeats it while holding the storage lifetime lease.
        helper('storage-check', os.environ['NATIVE_TCK_TEAM_ID'], sid,
               data=json.dumps(plan['storage'] or []))
        template = build_template(root, source, plan)
        if not state['stopped']:
            api('POST', f'/sandboxes/{sid}/pause', {'memory': False})
            state['stopped'] = True
            save(path, state)
        response = api('POST', f'/sandboxes/{sid}/resume', {
            'timeout': 900, 'kitTemplateID': template['templateID']})
        state.update(response)
        state.update(user=plan['identity']['name'], cwd=plan['cwd'], env=plan['env'],
                     selection=plan['selection'], source=source, stopped=False)
        save(path, state)
        return 0
    if verb == 'exec':
        if len(argv) < 4 or argv[2] != '--':
            raise ValueError('exec requires -- argv')
        return subprocess.run([os.environ['NATIVE_TCK_HELPER'], 'exec', str(path), *argv[3:]], timeout=120).returncode
    if verb == 'wait-idle':
        helper('disconnect', str(path))
        # Embed has no session-disconnect auto-stop; TTL is authoritative.
        return 0
    if verb == 'stop':
        api('POST', f'/sandboxes/{sid}/pause', {'memory': False})
        state['stopped'] = True
        save(path, state)
    elif verb == 'start':
        response = api('POST', f'/sandboxes/{sid}/resume', {'timeout': 900})
        state.update({k: v for k, v in response.items() if k in ('envdAccessToken', 'envdVersion')})
        state['stopped'] = False
        save(path, state)
    elif verb == 'status':
        if state['stopped']:
            # Observe the persisted pause record, rather than trusting adapter
            # state or treating every GET 404 as a stopped VM.
            record = api('GET', '/v2/sandboxes?state=paused')
            records = record if isinstance(record, list) else record.get('sandboxes', [])
            if not any(x.get('sandboxID') == sid for x in records):
                raise RuntimeError('paused sandbox is absent from public inventory')
            print('stopped')
        else:
            api('GET', f'/sandboxes/{sid}')
            print('running')
    elif verb == 'selection':
        print(json.dumps(state['selection']))
    elif verb == 'rm':
        api('DELETE', f'/sandboxes/{sid}')
        save(root / (argv[1] + '.removed.json'), {'sandboxID': sid})
        path.unlink()
    else:
        raise RuntimeError('native TCK verb is not wired: ' + verb)
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main(sys.argv[1:]))
    except Refused as exc:
        print(str(exc), file=sys.stderr)
        sys.exit(2)
    except Exception as exc:
        print(str(exc), file=sys.stderr)
        sys.exit(1)
