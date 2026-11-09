#!/usr/bin/env python3
"""Component smoke inside an isolated Linux container running the patched envd.
Exercises the same file staging and Process RPC path as the orchestrator.
Does not claim Firecracker/lifecycle/network conformance.
"""
import base64
import http.client
import json
import os
import struct
import uuid

HOST = os.environ.get("ENVD_HOST", "127.0.0.1:49983")
HEADERS = {"Authorization": "Basic cm9vdDo=", "Connect-Protocol-Version": "1"}
if os.environ.get("ENVD_ACCESS_TOKEN"):
    HEADERS["X-Access-Token"] = os.environ["ENVD_ACCESS_TOKEN"]


def request(path, body, content_type="application/json"):
    conn = http.client.HTTPConnection(HOST, timeout=15)
    conn.request("POST", path, body, {**HEADERS, "Content-Type": content_type})
    response = conn.getresponse()
    assert response.status == 200, (path, response.status, response.read())
    return response


def stream(method, message):
    raw = json.dumps(message).encode()
    return request("/process.Process/" + method,
                   b"\0" + struct.pack(">I", len(raw)) + raw,
                   "application/connect+json")


def event(response):
    header = response.read(5)
    assert len(header) == 5, "process stream ended unexpectedly"
    flags, length = header[0], struct.unpack(">I", header[1:])[0]
    assert length <= 4 << 20
    message = json.loads(response.read(length))
    assert not flags & 2, message
    return message["event"]


def rpc(method, message):
    response = request("/process.Process/" + method, json.dumps(message).encode())
    result = json.loads(response.read())
    response.close()
    return result


def identity(name):
    rows = [line.split(":") for line in open("/etc/passwd")]
    row = next(row for row in rows if row[0] == name)
    groups = [int(g[2]) for g in (line.strip().split(":") for line in open("/etc/group"))
              if name in g[3].split(",")]
    return dict(name=name, uid=int(row[2]), gid=int(row[3]), home=row[5], groups=groups)


def start(payload, tty=False):
    path = "/run/e2b-kit/" + str(uuid.uuid4()) + ".json"
    response = request("/files?username=root&path=" + path,
                       json.dumps(payload).encode(), "application/octet-stream")
    response.read()
    response.close()
    message = {"process": {"cmd": "/usr/bin/envd", "args": ["kit-execute", path]},
               "stdin": tty}
    if tty:
        message.update(tag="kit-workload", pty={"size": {"cols": 80, "rows": 24}})
    return stream("Start", message)


def collect(response):
    output = b""
    while True:
        e = event(response)
        if "data" in e:
            for value in e["data"].values():
                output += base64.b64decode(value)
        if "end" in e:
            response.close()
            return e["end"].get("exitCode", 0), output


user = identity("nobody")
base = dict(identity=user, cwd="/tmp", env={"PATH": "/usr/bin:/bin", "HOME": user["home"]})
exit_code, output = collect(start(dict(base, action="exec", argv=["/bin/sh", "-c", "id -u; exit 7"])))
assert exit_code == 7 and str(user["uid"]).encode() in output

path = "/tmp/kit-owned-" + str(uuid.uuid4())
exit_code, _ = collect(start(dict(action="write", identity=user,
                               file=dict(path=path, content="literal $HOME ; value", mode=384, overwrite=True))))
assert exit_code == 0 and os.stat(path).st_uid == user["uid"]
assert open(path).read() == "literal $HOME ; value"
os.unlink(path)

bad = {**user, "home": "/unexpected"}
exit_code, output = collect(start(dict(base, identity=bad, action="exec", argv=["/bin/true"])))
assert exit_code != 0 and b"identity changed" in output

# The first stream is deliberately detached. The process must stay alive, be
# discoverable by tag, and accept terminal input through a second connection.
response = start(dict(base, action="terminal", argv=["/bin/sh", "-c",
    'printf "KIT_PROMPT>"; read -r line; printf "\\nKIT_REPLY=%s\\n" "$line"; exit 0']), tty=True)
assert "start" in event(response)
response.close()
processes = rpc("List", {}).get("processes", [])
assert any(p.get("tag") == "kit-workload" for p in processes)
attached = stream("Connect", {"process": {"tag": "kit-workload"}})
assert "start" in event(attached)
rpc("Update", {"process": {"tag": "kit-workload"}, "pty": {"size": {"cols": 100, "rows": 30}}})
rpc("SendInput", {"process": {"tag": "kit-workload"},
                  "input": {"pty": base64.b64encode(b"literal ; $HOME\n").decode()}})
exit_code, output = collect(attached)
assert exit_code == 0 and b"KIT_REPLY=literal ; $HOME" in output, output
print("PASS: identity, exit status, file ownership, identity drift, PTY detach/attach, resize, literal input")

# Bundles remain complete, and a second start recognizes only recorded links.
import tempfile
import pathlib
skill_root = pathlib.Path(tempfile.mkdtemp(prefix="kit-skills-"))
os.chmod(skill_root, 0o777)
source = skill_root / "bundle"
source.mkdir()
(source / "SKILL.md").write_text("Read scripts/tool on demand.\n")
(source / "scripts").mkdir()
(source / "scripts/tool").write_text("#!/bin/sh\nexit 99\n")
os.chmod(source / "scripts/tool", 0o755)
skills = {"bundles": [{"source": str(source), "name": "review"}],
          "destinations": [str(skill_root / "first"), str(skill_root / "second")],
          "report": str(skill_root / "state/skills.json")}
for _ in range(2):
    code, output = collect(start(dict(base, action="skills", skills=skills)))
    assert code == 0, output
for dest in skills["destinations"]:
    assert pathlib.Path(dest, "review/SKILL.md").read_text().startswith("Read scripts/tool")
    assert os.stat(pathlib.Path(dest, "review/scripts/tool")).st_mode & 0o111
report = json.loads(pathlib.Path(skills["report"]).read_text())
assert report["hostSharing"] == "off" and not report["results"][0].get("skipped")
assert os.stat(skills["report"]).st_uid == user["uid"]
# An existing unrecorded entry is preserved; optional failure is recorded.
conflict = skill_root / "conflict"
conflict.mkdir(mode=0o777)
(conflict / "review").write_text("user skill")
skills["destinations"] = [str(conflict)]
code, output = collect(start(dict(base, action="skills", skills=skills)))
assert code != 0 and b"conflict" in output, output
skills["bundles"][0]["optional"] = True
code, output = collect(start(dict(base, action="skills", skills=skills)))
assert code == 0, output
assert (conflict / "review").read_text() == "user skill"
report = json.loads(pathlib.Path(skills["report"]).read_text())
assert "conflict" in report["results"][0]["skipped"]
print("PASS: native skills exposure, executable preservation, restart, conflicts, optional skips, workload UID")

if os.path.exists("/adapter.test"):
    code, output = collect(start(dict(base, action="exec", argv=["/adapter.test", "-test.run", "^(TestACPStdioCommandEnvironmentAndProtocol|TestLegacyContextProfilesAndAttribution|TestResourceSizingMaximaAndRefusal)$", "-test.v"])))
    assert code == 0, output
    print("PASS: compiled ACP stdio launcher, context attribution and resource planner in the VM")

# Mount through exactly the privileged envd request path used before hooks.
scratch = "/tmp/kit-tmpfs-" + str(uuid.uuid4())
os.mkdir(scratch)
open(scratch + "/hidden-image-file", "w").write("hidden")
tmpfs = dict(action="tmpfs", identity=user,
             tmpfs=dict(path=scratch, mode=0o1777, bytes=1048576))
exit_code, output = collect(start(tmpfs))
assert exit_code == 0, output
assert os.listdir(scratch) == []
assert os.stat(scratch).st_uid == user["uid"]
assert os.stat(scratch).st_mode & 0o7777 == 0o1777
assert any(line.split()[4] == scratch and " - tmpfs " in line
           for line in open("/proc/self/mountinfo"))
open(scratch + "/discard", "w").write("scratch")
exit_code, output = collect(start(tmpfs))
assert exit_code != 0 and b"already mounted" in output, output
import ctypes
assert ctypes.CDLL(None).umount(scratch.encode()) == 0
exit_code, output = collect(start(tmpfs))
assert exit_code == 0 and os.listdir(scratch) == [], output
assert ctypes.CDLL(None).umount(scratch.encode()) == 0
print("PASS: native tmpfs through envd, initial emptiness, ownership, mode, conflict refusal and reset")
