#!/usr/bin/env python3
"""Build native Kit templates and attach their generic PTY workload (e2b 2.46.0)."""
import argparse
import fcntl
import pathlib
import re
import os
import select
import signal
import sys
import termios
import threading
import time
import tty
import urllib.error
import urllib.request
import json

from e2b import Sandbox, PtySize
from e2b.sandbox.commands.command_handle import CommandExitException


def api(method, path, body=None):
    url = os.environ["E2B_API_URL"].rstrip("/") + path
    req = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(), method=method,
        headers={"X-API-Key": os.environ["E2B_API_KEY"], "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as response:
        raw = response.read()
        return json.loads(raw) if raw else None


def build(args):
    bindings = {}
    for binding in args.secret:
        service, separator, name = binding.partition("=")
        if not separator or not service or not name or service in bindings:
            raise ValueError("--secret requires one SERVICE=SECRET_NAME binding per service")
        bindings[service] = name
    allocation = api("POST", "/v3/templates", {"name": args.name, "cpuCount": args.cpus, "memoryMB": args.memory_mib})
    template_id, build_id = allocation["templateID"], allocation["buildID"]
    source = {"references": args.references, "options": {
        "ephemeralVolumes": args.ephemeral_volumes,
        "ignoreAgentSessions": args.ignore_agent_sessions,
        "hostOnlyNetwork": args.host_only_network, "secretBindings": bindings}}
    api("POST", f"/v2/templates/{template_id}/builds/{build_id}", {"fromKit": source})
    deadline = time.monotonic() + 1800
    while time.monotonic() < deadline:
        state = api("GET", f"/templates/{template_id}/builds/{build_id}/status")
        if state["status"] == "ready":
            print(template_id)
            return
        if state["status"] == "error":
            raise RuntimeError(f"Kit template build failed: {state}")
        time.sleep(2)
    raise TimeoutError(f"build still pending: template={template_id} build={build_id}")


def workload(sandbox):
    candidates = [p for p in sandbox.commands.list() if p.tag == "kit-workload"]
    if len(candidates) != 1:
        raise RuntimeError("expected one running kit-workload PTY; inspect sandbox process logs")
    return candidates[0].pid


def attach(sandbox):
    pid = workload(sandbox)
    handle = sandbox.pty.connect(pid, timeout=0)
    stop = threading.Event()
    errors = []
    # Open a fresh descriptor; inherited stdin/stdout may be nonblocking on macOS.
    with open("/dev/tty", "r+b", buffering=0) as terminal:
        fd = terminal.fileno()
        saved = termios.tcgetattr(fd)
        print("Attached. Ctrl-] detaches; the workload keeps running.", file=sys.stderr)
        def write(data):
            remaining = memoryview(data)
            while remaining:
                select.select([], [fd], [])
                try:
                    remaining = remaining[os.write(fd, remaining):]
                except BlockingIOError:
                    continue
        def input_loop():
            size = None
            try:
                while not stop.is_set():
                    current = os.get_terminal_size(fd)
                    if current != size:
                        sandbox.pty.resize(pid, PtySize(cols=current.columns, rows=current.lines))
                        size = current
                    if not select.select([fd], [], [], 0.2)[0]:
                        continue
                    data = os.read(fd, 4096)
                    if not data or b"\x1d" in data:
                        os.kill(os.getpid(), signal.SIGINT)
                        return
                    sandbox.pty.send_stdin(pid, data)
            except Exception as exc:
                errors.append(exc)
                os.kill(os.getpid(), signal.SIGINT)
        try:
            tty.setraw(fd)
            worker = threading.Thread(target=input_loop, daemon=True)
            worker.start()
            try:
                result = handle.wait(on_pty=write)
                return result.exit_code
            except KeyboardInterrupt:
                if errors:
                    raise errors[0]
                return 0
            except CommandExitException as exc:
                return exc.exit_code
        finally:
            stop.set()
            termios.tcsetattr(fd, termios.TCSADRAIN, saved)
            handle.disconnect()


def smoke(template):
    # This command specifically tests the published shell Kit. Other Kits use attach.
    sandbox = Sandbox.create(template, timeout=120)
    try:
        pid = workload(sandbox)
        handle = sandbox.pty.connect(pid, timeout=30)
        output = bytearray()
        sandbox.pty.send_stdin(pid, b"test -t 0 && printf '\\nNATIVE_KIT_TTY_OK\\n'; exit\n")
        handle.wait(on_pty=output.extend)
        assert b"\r\nNATIVE_KIT_TTY_OK\r\n" in output, output
        print("PASS: native shell create, workload discovery, PTY input/output, exit")
    finally:
        sandbox.kill()


def ports(args):
    """Read the host-owned registry; unlocked crash leftovers are not live ports."""
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", args.sandbox):
        raise ValueError("invalid sandbox ID")
    root = pathlib.Path(args.registry)
    if not root.is_dir():
        raise ValueError("port registry not found; run this command on the Embed host")
    records = []
    for team in root.iterdir():
        if not team.is_dir() or (args.team and team.name != args.team):
            continue
        directory = team / args.sandbox
        if not directory.is_dir():
            continue
        for path in directory.glob("*.json"):
            try:
                with path.open() as stream:
                    try:
                        fcntl.flock(stream, fcntl.LOCK_SH | fcntl.LOCK_NB)
                    except BlockingIOError:
                        record = json.load(stream)
                        records.append(record)
                    # An unlocked record survived a stopped/crashed publisher.
            except FileNotFoundError:
                continue
    if args.json:
        print(json.dumps(records, indent=2))
        return
    for record in records:
        for port in record["endpoints"]:
            print(f'{port.get("name") or "-"}\t{port["container"]}/{port["transport"]}'
                  f'\t{port["family"]}\t{port["address"]}')
    if not records:
        print("No active native port publications.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    p = commands.add_parser("build")
    p.add_argument("references", nargs="+")
    p.add_argument("--name", default="native-kit")
    p.add_argument("--cpus", type=int, default=2)
    p.add_argument("--memory-mib", type=int, default=512)
    p.add_argument("--ephemeral-volumes", action="store_true")
    p.add_argument("--ignore-agent-sessions", action="store_true", help="deprecated no-op; session declarations are accepted by default")
    p.add_argument("--host-only-network", action="store_true")
    p.add_argument("--secret", action="append", default=[], metavar="SERVICE=SECRET_NAME", help="bind a credential service to a host secret name; never pass the value")
    p = commands.add_parser("create")
    p.add_argument("template")
    p.add_argument("--ttl", type=int, default=900)
    for command in ("attach", "kill"):
        commands.add_parser(command).add_argument("sandbox")
    p = commands.add_parser("recreate", help="replace a Kit instance filesystem, retaining its volumes")
    p.add_argument("sandbox")
    p.add_argument("template", help="accessible pristine native Kit template ID")
    p.add_argument("--ttl", type=int, default=900)
    p = commands.add_parser("ports", help="list native publications on the Embed host")
    p.add_argument("sandbox")
    p.add_argument("--registry", default=os.environ.get("NATIVE_KIT_PORTS_DIR", "/var/lib/e2b/kit-ports"))
    p.add_argument("--team", help="restrict listing to this team UUID")
    p.add_argument("--json", action="store_true")
    commands.add_parser("smoke").add_argument("template")
    args = parser.parse_args()
    if args.command == "build":
        build(args)
    elif args.command == "create":
        print(Sandbox.create(args.template, timeout=args.ttl).sandbox_id)
    elif args.command == "attach":
        return attach(Sandbox.connect(args.sandbox))
    elif args.command == "recreate":
        api("POST", f"/sandboxes/{args.sandbox}/pause", {"memory": False})
        result = api("POST", f"/sandboxes/{args.sandbox}/resume", {
            "kitTemplateID": args.template, "timeout": args.ttl})
        print(result["sandboxID"])
    elif args.command == "ports":
        ports(args)
    elif args.command == "kill":
        Sandbox.connect(args.sandbox).kill()
    else:
        smoke(args.template)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (Exception, KeyboardInterrupt) as exc:
        print(f"native-kit: {exc}", file=sys.stderr)
        sys.exit(1)
