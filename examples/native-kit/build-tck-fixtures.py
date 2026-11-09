#!/usr/bin/env python3
"""Publish the real TCK fixtures to an explicitly supplied loopback registry.

--base-image is a locally provisioned Linux Kit image. Replacing fixture FROM
images permits the native Embed guest prerequisites to be installed in advance.
The workload recipes, probes and descriptors otherwise come from the supplied
TCK source tree. This publishes only test artifacts to a loopback registry.
"""
import argparse
import json
import pathlib
import re
import shutil
import subprocess
import tempfile


def fixture_reference(registry, name):
    # The update check supplies two revisions of one published repository.
    # Giving them different repositories would test isolation, not retention.
    revisions = {'host-mount-version-v1': '1.0.0', 'host-mount-version-v2': '2.0.0'}
    if name in revisions:
        return registry + '/host-mount-version:' + revisions[name]
    return registry + '/' + name + ':1.0.0'


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--fixtures', type=pathlib.Path, required=True)
    p.add_argument('--helper', required=True)
    p.add_argument('--base-image', required=True)
    p.add_argument('--registry', default='localhost:5112/native-tck')
    p.add_argument('--catalog', type=pathlib.Path, required=True)
    args = p.parse_args()
    if not re.fullmatch(r'(localhost|127\.0\.0\.1):[0-9]+/[a-z0-9/_-]+', args.registry):
        raise ValueError('fixture publication is restricted to a loopback registry')
    catalog = {}
    with tempfile.TemporaryDirectory(prefix='native-tck-fixtures-') as scratch:
        for directory in sorted(args.fixtures.iterdir()):
            name = directory.name
            authored = directory / (name + '.yaml')
            if not authored.is_file():
                continue
            descriptor = json.loads(subprocess.check_output([args.helper, 'describe', str(authored)]))
            context = pathlib.Path(scratch) / name
            shutil.copytree(directory, context)
            recipe = descriptor.get('build')
            if not recipe:
                companion = directory / descriptor.get('dockerfile', name + '.dockerfile')
                recipe = companion.read_text() if companion.exists() else None
            image = '-'
            if recipe:
                recipe = recipe.replace('FROM docker/sandbox-templates:claude-code-docker', 'FROM ' + args.base_image)
                recipe = recipe.replace('FROM busybox:1.37 AS stage', 'FROM ' + args.base_image + ' AS stage\nUSER root')
                if 'FROM python:3.13-alpine' in recipe:
                    recipe = recipe.replace('FROM python:3.13-alpine', 'FROM ' + args.base_image)
                    recipe = recipe.replace('RUN apk add --no-cache bash curl git ca-certificates && adduser -D -u 1000 agent',
                                            'USER root\nRUN ln -s /usr/bin/python3 /usr/local/bin/python3')
                (context / 'Dockerfile').write_text(recipe)
                image = 'native-tck-' + name + ':local'
                subprocess.run(['docker', 'build', '--tag', image, str(context)], check=True, stdout=__import__('sys').stderr)
            elif descriptor['kind'] == 'workload':
                raise ValueError('workload has no fixture recipe: ' + name)
            target = fixture_reference(args.registry, name)
            subprocess.run([args.helper, 'publish', str(directory), image, target], check=True)
            catalog[name] = target
            print('PUBLISHED', name, flush=True)
            args.catalog.write_text(json.dumps(catalog, indent=2))


if __name__ == '__main__':
    main()
