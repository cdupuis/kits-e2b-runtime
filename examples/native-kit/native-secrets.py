#!/usr/bin/env python3
"""Administer the local Embed host store; no secret values in argv or output."""
import argparse
import fcntl
import getpass
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile
import uuid


def private_file(path, flags):
    fd = os.open(path, flags | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.geteuid():
        os.close(fd)
        raise ValueError("store and lock must be private regular files owned by this user")
    return fd


def update(path, project, name, value):
    path = Path(path)
    if not path.is_absolute():
        raise ValueError("store path must be absolute")
    project = str(uuid.UUID(project))
    if project == str(uuid.UUID(int=0)):
        raise ValueError("project must be a nonzero UUID")
    name = name.lower()
    if not re.fullmatch(r"[a-z0-9_-]{1,128}", name) or name.startswith("sec_"):
        raise ValueError("invalid secret name")
    if value is not None and (not value or len(value.encode()) > 65536 or
                              any((ord(c) < 32 and c != "\t") or ord(c) == 127 for c in value)):
        raise ValueError("secret must be a nonempty HTTP header value, at most 64 KiB")
    # The directory is provisioned by the administrator, never guest-controlled.
    info = path.parent.stat()
    if info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise ValueError("store directory must be owned by this user and mode 0700")
    with os.fdopen(private_file(str(path) + ".lock", os.O_RDWR | os.O_CREAT), "r+") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            with os.fdopen(private_file(path, os.O_RDONLY), "r") as source:
                raw = source.read(1048577)
                if len(raw.encode()) > 1048576:
                    raise ValueError("store exceeds 1 MiB")
                document = json.loads(raw)
        except FileNotFoundError:
            document = {"projects": {}}
        secrets = document["projects"].setdefault(project, {})
        if value is None:
            secrets.pop(name, None)
        else:
            secrets[name] = value
        raw = json.dumps(document).encode()
        if len(raw) > 1048576:
            raise ValueError("store exceeds 1 MiB")
        fd, temporary = tempfile.mkstemp(prefix=".native-secrets-", dir=path.parent)
        try:
            with os.fdopen(fd, "wb") as target:
                target.write(raw)
                target.flush()
                os.fsync(target.fileno())
            os.replace(temporary, path)
            directory = os.open(path.parent, os.O_DIRECTORY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--store", required=True)
    parser.add_argument("--project", required=True, help="authenticated E2B team/project UUID")
    parser.add_argument("operation", choices=["set", "delete"])
    parser.add_argument("name")
    args = parser.parse_args()
    value = None
    if args.operation == "set":
        # Refuse getpass's echoed stdin fallback in automation.
        with open("/dev/tty", "r+") as terminal:
            if not terminal.isatty():
                raise ValueError("setting a secret requires a terminal")
            value = getpass.getpass("Secret value: ", stream=terminal)
    update(args.store, args.project, args.name, value)
    print("Host store updated.")


if __name__ == "__main__":
    try:
        main()
    except (Exception, KeyboardInterrupt):
        # JSON/OS exceptions may contain file data: do not print them.
        print("native-secrets: update failed; check arguments and private store permissions", file=sys.stderr)
        sys.exit(1)
