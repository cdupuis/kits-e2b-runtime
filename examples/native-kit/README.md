# Native Kit v3 prototype

This patch makes Kit references a template source in E2B's OSS runtime. The
orchestrator resolves/composes the artifacts, keeps their declarations in template
metadata, and executes their lifecycle through an executor built into envd.
The declared workload runs in a normal envd PTY tagged `kit-workload`.
No standalone `kit-e2b` runtime or uploaded guest executable is needed.

**This is an experimental source prototype, not a conformant or production release.**
Use an isolated deployment with the patched API, orchestrator and envd together.
Older workers do not understand the Kit metadata. Local Embed validation now
uses the patched API, orchestrator and envd with Firecracker v1.14-0.2.0 and
Linux 6.1.177 on an ARM64 host with 4 KiB pages, usable KVM, loop devices and NBD.
Template builds, interactive workloads, storage, ports and both snapshot resume
modes have been exercised. See Validation below for the tested scope.

The candidate [TCK runner](TCK.md) exercises official fixtures through the public
API and envd and reports the explicitly selected capability subset.

## Sources and build

The runtime feature branch is published in
[cdupuis/kits-e2b-runtime](https://github.com/cdupuis/kits-e2b-runtime/tree/feat/native-kit-v3).
Its Go modules pin the planner and platform-aware loader from
[docker/kits-e2b](https://github.com/docker/kits-e2b/tree/feat/native-runtime-support)
at `f23e47e89c004828d1258c9029d140c3f1f1db11`. No sibling checkout is required.

The adapter repository is internal. Builds require GitHub access to it and
Git HTTPS or SSH authentication configured for Go's Git subprocesses. The build
helper sets `GOPRIVATE=github.com/docker/kits-e2b` (preserving existing entries)
to keep this module off public module proxies and checksum services. A fork's
standard GitHub Actions token cannot be assumed to read Docker's internal
repository; CI needs separately provisioned read access. Never commit a token.

```sh
git clone --branch feat/native-kit-v3 https://github.com/cdupuis/kits-e2b-runtime.git
cd kits-e2b-runtime
examples/native-kit/build.sh
```

This builds Linux binaries for the current architecture under
`examples/native-kit/bin/`. Set `NATIVE_KIT_ARCH=amd64` or `arm64` to select a
target. Orchestrator requires CGO and a matching C compiler; build on the target
architecture or provide the appropriate cross-compiler. The helper uses the
committed Go workspace without editing it. For adapter development only, set
`KIT_E2B_SOURCE=/absolute/path/to/kit-e2b` to add that checkout to a temporary
workspace instead of using the published adapter pin.

For Embed, first follow `embed/compose/README.md` on an isolated Linux KVM host.
Its release artifacts are unpatched. Stop API and orchestrator, then install the
locally built orchestrator at the host path `/var/lib/e2b/bin/orchestrator` and
envd at `/fc-envd/envd`. Add an API volume override mapping the absolute path of the built API binary
to `/api:ro`; the release container runs `./api` from `/`. Preserve the existing
Compose API environment, entrypoint, mounts and secrets. Restart only those two
services with `--no-deps`;
re-running `fetch-artifacts` replaces local binaries with release artifacts.
Build a new template afterward; old templates contain the old envd.
These deployment steps need validation on the KVM host; they were not executed here.

## Build, create, attach

Install `e2b==2.46.0` in a Python virtual environment. Export `E2B_API_URL`,
`E2B_SANDBOX_URL` and `E2B_API_KEY` from the patched Embed deployment's ready output.
Do not point this prototype at the managed E2B API; it does not have `fromKit`.

```sh
python examples/native-kit/native-kit.py build \
  --name native-shell docker.io/docker/sbx-kit-shell:1.0.0
# Copy the printed template ID:
python examples/native-kit/native-kit.py create TEMPLATE_ID --ttl 900
# Copy the printed sandbox ID:
python examples/native-kit/native-kit.py attach SANDBOX_ID
python examples/native-kit/native-kit.py kill SANDBOX_ID
```

Ctrl-] detaches. Resize and input use E2B's existing PTY APIs. Attaching does not
launch another workload. Standard `commands.run` remains available for generic
commands; it does not automatically compile the Kit's workload environment.

Equivalent build payload to `POST /v2/templates/{templateID}/builds/{buildID}`:

```json
{"fromKit":{"references":["docker.io/docker/sbx-kit-shell:1.0.0"]}}
```

Multiple references compose in the supplied order. Allocate the template/build
first through `POST /v3/templates`; the example handles allocation and polling.
`fromKit` excludes image/template sources, build steps, start/ready commands and
registry overrides. This prototype admits public registry artifacts only and
never reads the daemon's Docker credentials.

## Lifecycle and policy

- Reusable templates retain the resolved plan and image identities. They run no
  Kit install/startup hooks or Kit workload. E2B's normal OS provisioning remains,
  but its default-user/sudo/home ownership and writable-directory convenience
  changes are skipped for Kits.
- Creation starts unroutable with closed Kit egress. Install hooks receive their
  install environment and grants. An instance-only filesystem snapshot and a
  fresh VM boot discard install processes and connections before runtime grants.
- Startup hooks, files and guidance precede the declared workload. envd verifies
  account identities and drops UID/GID/supplementary groups in a child process.
- Memory resume preserves the running workload. Filesystem resume reruns startup
  and restarts the workload, without repeating install. Installed snapshots only
  resume their original sandbox ID; forks must start from the original Kit template.
- The host owns lifecycle state and network grants. Kit sandbox egress updates,
  external environment/volume overrides, and deriving an ordinary template from
  a Kit template are refused. No install flag is trusted from the guest.
- Exact hostname grants use CE's HTTP Host/TLS SNI proxy. The host resolves
  `/etc/hosts`; strict firewall rules close DNS, UDP, IPv6 and platform exceptions.
  The CE proxy serves these grants on ports 80/443. Wildcards, IP grants and
  unsupported contracts are refused by the planner. `--host-only-network` is an
  explicit relaxation of port constraints; it is never inferred.
- `--ephemeral-volumes` opts into ordinary sandbox disk instead of managed volumes.
  Both agent-session capability types are accepted as metadata by default, even
  when required. Session-management operations remain unimplemented. The old
  `--ignore-agent-sessions` option remains a deprecated no-op.

## Storage on local Embed

Set `NATIVE_KIT_STORAGE_ROOT` on **both the API and orchestrator** to the
same absolute, dedicated directory. Create it with mode `0700` and mount
it at the same path in both services. For example, merge this into the
existing Compose overrides:

```yaml
services:
  api:
    environment:
      NATIVE_KIT_STORAGE_ROOT: /var/lib/e2b/kit-storage
    volumes:
      - /var/lib/e2b/kit-storage:/var/lib/e2b/kit-storage
  orchestrator:
    environment:
      NATIVE_KIT_STORAGE_ROOT: /var/lib/e2b/kit-storage
    volumes:
      - /var/lib/e2b/kit-storage:/var/lib/e2b/kit-storage
```

Create the host directory first with
`sudo install -d -m 0700 /var/lib/e2b/kit-storage`. The services need
permission to manage it, including ownership changes. Build a new Kit
template after enabling storage. The builder derives support from worker
configuration; a client `nativeStorage` option cannot enable it. Without
this configuration, required storage capabilities fail and optional ones
are skipped, unless the existing explicit volume compatibility override
is used. Omit `--ephemeral-volumes` to get persistent volume behavior.

- `volume@1`: an empty host directory per tenant, sandbox ID and cleaned
  destination, exported through E2B's NFS proxy before hooks. Reattaching
  the same instance preserves contents and permissions. Omitted paths
  remain retained but unmounted. Equivalent size/mode requests reattach;
  incompatible requests fail before allocation. Deleting the instance
  removes its private directories, including omitted ones. API timeout kills
  remove private data only when no earlier pause snapshot remains resumable.
  E2B retains that snapshot after a resumed VM expires, so its private data
  remains until explicit DELETE. An uncertain snapshot lookup also retains
  data. Pause retains data.
- `tmpfs: true`: RAM-backed scratch mounted through envd before hooks and
  mounted fresh after the install cold boot or filesystem resume. Size,
  mode and workload ownership are applied. Memory pause/resume suspends
  the same VM and preserves its RAM, including tmpfs.
- `host-mount@1`: a directory per tenant, canonical published Kit repository
  and destination. Tags and digests do not change that identity. Concurrent
  instances of the same Kit share it; deletion of the last instance keeps
  it. Kits cannot choose arbitrary host paths. This is storage on the Embed
  host, not a bind mount from the SDK caller's laptop.

Host-shared locations are indexed in
`/var/lib/e2b/kit-storage/shared/*.json`. Each entry includes `kit`,
`volume.path` and `directory`; for example:

```sh
sudo sh -c 'jq "{kit, destination: .volume.path, directory}" /var/lib/e2b/kit-storage/shared/*.json'
```

Use the reported directory to read or edit files on the host. The paired storage
library also exposes `Shared`, `ReadShared` and `RemoveShared` for host
administration. Listing accepts tagged or digested Kit references and resolves
their canonical repository. Reads stay within the share, accept only regular
files, and are limited to 1 MiB per observation. Removal takes a listed handle
and refuses shares still referenced by any instance, including paused instances.
Delete those instances before resetting their share. The native TCK helper exposes
these operations as `host-mounts TEAM KIT_REF`, `host-mount-read TEAM HANDLE PATH`
and `host-mount-rm TEAM HANDLE`; see [TCK.md](TCK.md) for local validation setup.
Do not remove registry or private volume directories while instances exist.

This backend targets one local Embed host with durable local storage.
API and workers must share the registry and locking filesystem; this is
not a distributed storage scheduler. Persistent NFS size is advisory;
initial mode and ownership are applied once. NFS semantics, rather than
local block-device semantics, apply. Tmpfs ancestors of NFS destinations
are refused because envd attaches NFS before guest scratch mounts. Root
mounts are refused. Export names are generated by the runtime, and the
reserved `native-kit` backend cannot be selected through client volume IDs.
The firewall permits only TCP 111/2049 to the control address for NFS,
including during the install checkpoint; other platform egress stays
closed.

The public resume API supports replacing a paused native Kit composition
under the same sandbox ID with `kitTemplateID`; see the recreation example
below. Local Embed create, both pause/resume modes, instance isolation, and
active/paused deletion have been exercised with real NFS and tmpfs mounts.
Component checks also cover NFS proxy write confinement and packet-level
firewall rules. The pinned TCK runner and its tested scope are documented
in [TCK.md](TCK.md).

## Published ports on local Embed

`port@1` publishes raw TCP or UDP services on automatically allocated IPv4
loopback ports on the Embed host. HTTP and WebSocket services work through
TCP forwarding too. Services should listen on `0.0.0.0` inside the guest.
`family: ipv4` (the default) and `family: dual` are supported; both deliver
over IPv4. Required IPv6-only entries fail admission; optional ones skip.

Enable publication on the orchestrator and build a new Kit template:

```sh
sudo install -d -m 0700 /var/lib/e2b/kit-ports
```

Merge this into the existing Compose override. Embed's orchestrator uses
host networking, so its loopback listeners belong to the Embed host:

```yaml
services:
  orchestrator:
    environment:
      NATIVE_KIT_PORTS_DIR: /var/lib/e2b/kit-ports
    volumes:
      - /var/lib/e2b/kit-ports:/var/lib/e2b/kit-ports
```

A Kit can request:

```yaml
- type: com.docker.sandbox/port@1
  config:
    name: devserver
    container: 3000
    transport: tcp
    family: ipv4
```

After create, list its actual bindings **on the Embed host**, using the
Python environment where the E2B SDK is installed:

```sh
sudo "$(command -v python)" examples/native-kit/native-kit.py ports SANDBOX_ID
# Example: devserver    3000/tcp    ipv4    127.0.0.1:42117
curl http://127.0.0.1:42117/
```

`--json` returns structured records; `--team UUID` restricts the host-side
listing. The optional name is retained. The registry is host-owned and
requires local permission to read; the guest cannot supply endpoints.
Listings ignore unlocked records left by a crashed process.

For a remote Embed host, forward a listed TCP binding through SSH:

```sh
ssh -N -L 8080:127.0.0.1:42117 user@embed-host
# Then open http://127.0.0.1:8080 locally.
```

Forwarding starts after Kit initialization. Pausing/stopping/deleting the
VM closes listeners and established sessions before its network slot can
be reused. A resumed or replaced VM gets newly allocated ports; list them
again. An in-place checkpoint keeps its existing VM and bindings.
UDP maps clients to separate upstream sockets, with a one-minute idle
expiry. The prototype caps each instance at 128 publications and 1024
tracked sockets; each TCP flow uses two sockets. Loopback access follows
host trust and adds no HTTP authentication layer.

This handler does not implement `host-port@1`. E2B's existing HTTP ingress
proxy remains separate from these local bindings. The managed-cloud
adapter refuses native-port bundles. With publication disabled, required
`port@1` entries fail and optional entries are skipped.

For component validation, build a static `pkg/kitports` test binary and run
`firecracker-ports-smoke.sh` with `FIRECRACKER`, `KERNEL` and `PORTS_TEST`
set. It boots raw TCP/UDP services behind an isolated TAP and exercises
allocated loopback forwarding. It requires root network administration,
`/dev/net/tun`, Docker and KVM; it does not require NBD or change the host's
ordinary network routes. Full Embed lifecycle/TCK validation is still
outstanding.

## Host-side credentials (local Embed)

The opt-in native injector resolves named bindings on the orchestrator host and
replaces outbound HTTPS headers for the Kit's declared credential destinations.
Guest environments contain `proxy-managed` sentinels and `SBX_CRED_*_MODE=none`.
Only the public proxy CA enters the VM. Node receives `NODE_EXTRA_CA_CERTS`
pointing at envd's system CA bundle, so Claude can use the injected credential.

This local backend is separate from E2B Cloud's managed secret store. The OSS
`/secrets` API contains a management contract, but no plaintext resolver for CE;
setting `SECRETS_STORE_BACKEND_GRPC_ADDRESS` alone cannot enable this injector.
The local store is a private plaintext host file; protect it like other deployment
secrets. It is never included in templates, guest requests, snapshots or this repo.

On the isolated Embed host, create a private directory and a dedicated CA (run as
the user that runs orchestrator, normally root):

```sh
install -d -m 700 /var/lib/e2b/native-credentials
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout /var/lib/e2b/native-credentials/ca.key \
  -out /var/lib/e2b/native-credentials/ca.crt \
  -days 365 -subj '/CN=Native Kit Embed Egress/' \
  -addext 'basicConstraints=critical,CA:TRUE' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign'
chmod 600 /var/lib/e2b/native-credentials/ca.key

# PROJECT_UUID is the authenticated E2B team UUID, not a sandbox/template ID.
# The command prompts for the value without echoing it.
python3 examples/native-kit/native-secrets.py \
  --store /var/lib/e2b/native-credentials/secrets.json \
  --project PROJECT_UUID set anthropic-key
```

Mount the **directory** read-only into orchestrator (not the individual JSON file,
which is atomically replaced on rotation), and configure these environment variables
in its Compose service. A separate template-builder worker needs the same injector
configuration for capability admission. Restart the workers after configuration:

```text
NATIVE_KIT_SECRETS_FILE=/var/lib/e2b/native-credentials/secrets.json
NATIVE_KIT_CA_CERT_FILE=/var/lib/e2b/native-credentials/ca.crt
NATIVE_KIT_CA_KEY_FILE=/var/lib/e2b/native-credentials/ca.key
```

All three are required together; a missing, invalid or publicly readable private
file fails startup. Use the same dedicated CA across workers so memory-resumed
clients retain trust. CA rotation needs worker restart and fresh sandbox processes;
secret-value rotation does not. Provision the store on every eligible worker.

Build a Kit with a named service binding:

```sh
python examples/native-kit/native-kit.py build --name native-claude \
  --secret anthropic=anthropic-key \
  --ephemeral-volumes --host-only-network \
  docker.io/docker/sbx-kit-claude:1.0.0
python examples/native-kit/native-kit.py create TEMPLATE_ID
python examples/native-kit/native-kit.py attach SANDBOX_ID
```

The compatibility flags discard managed-volume contracts and relax port bounds.
Agent-session declarations are accepted without implementing session management. This uses the same generic workload and attach path as every other Kit.
The Claude example has not yet been exercised in a Firecracker VM.

Run `native-secrets.py ... set anthropic-key` again to rotate, or replace `set`
with `delete` to revoke. Each request rereads the store; an already authorized
in-flight request may finish. Missing bindings fail create/resume before boot and
fail subsequent outbound requests closed. Templates contain only secret names;
resolution uses the **creating project's** UUID, including for shared templates.
Installed instances do not require expired install-only credentials on resume.

Credential destinations require HTTPS on port 443 with matching SNI and HTTP Host.
The proxy verifies upstream certificates, uses host DNS, rejects private upstream
addresses, strips hop headers, refuses CONNECT/upgrade, and never follows redirects.
It supports HTTP/1.1 and streaming responses; HTTP/2-only or certificate-pinned clients
are unsupported. Current phase policy is checked on every intercepted request.
Without the configured injector, credential-bearing builds and creates are refused.


## Validation

Executed here:

- Public REST create cancellation during a controlled install HTTPS request and
  during the startup HTTPS request, after the install snapshot/cold boot. Both
  cases remove the VM, NBD attachment and private storage, without publishing
  ports or a publicly routable instance.
- Public REST boundary tests: guest-visible CPU/memory allocation and workload
  UID/GID, HOME, WORKDIR and non-init process identity. Create-time environment
  and network overrides fail before boot. Network widening/reset updates fail
  without changing saved state; subsequent filesystem resume and phase-scoped
  HTTPS still work. The API now applies a bounded node update before saving
  network state, preventing refused declarations from poisoning resume.
- TTL expiry removes VM/NBD resources and TCP/UDP listeners. A fresh instance
  with no pause snapshot releases private storage and cannot resume. An instance
  with a retained pause snapshot keeps private data, including changes made after
  that snapshot, and resumes with it intact. Explicit DELETE then removes private
  storage and resumability. API network/checkpoint/storage regressions pass with
  the race detector against isolated Redis/PostgreSQL test data.
- Published `docker.io/docker/sbx-kit-shell:1.0.0` resolution/composition on arm64.
- Real guest-facing and upstream TLS component tests for named credential injection,
  project isolation, rotation/revocation, header spoofing, redirects, authority checks,
  private-address denial and untrusted upstream certificates.
- Unit tests for source admission, lifecycle phase order/failures, snapshot metadata,
  deprecated-metadata refusal, account identity checks, and executing native
  finalization against fixtures to verify it preserves accounts and file permissions.
- Patched envd running in Docker: actual file upload and Process RPCs, UID/file
  ownership, exit status, identity drift, PTY detach/reattach/resize and literal input.
- Privileged Docker network namespaces: real nftables packet test proving DNS,
  UDP and platform bypasses are closed, TCP reaches the proxy path, and slot reset
  restores the ordinary policy.
- Local Embed API, patched orchestrator and envd on ARM64 with 4 KiB host pages:
  real `fromKit` template build and sandbox create; per-instance install followed
  by a different VM boot for startup; generic PTY input/output, resize, detach
  and reattach beyond the create deadline; workload UID/GID; context, bundled
  skill discovery and ACP stdio; NFS private/shared mounts and tmpfs; TCP/UDP
  loopback traffic before and after both snapshot resume modes. Memory resume
  retains the boot, workload PID/environment and tmpfs; filesystem resume
  cold-boots, reruns startup only and resets tmpfs. Delete closes port listeners
  and removes private storage; a fresh instance receives a fresh private volume
  while the shared directory survives. Installed snapshots refuse another ID.
  The test fixture derives from the published shell Kit and preinstalls Embed's
  OS packages during its container build, because the outer sandbox cannot
  reach the default guest DNS resolver. This does not establish an unmodified
  published shell template build in that network environment.

- Full local Embed credential path through real Firecracker guests: install and
  runtime HTTPS authenticate using injected headers despite guest forgeries and
  hop-header nominations. The guest trusts the public CA and sees phase-specific
  sentinels, with helper mode disabled. Rotation and revocation take effect on an
  existing guest TLS connection; redirects are returned and HTTP authority
  mismatches refused. A second project cannot use the first project's values and
  resolves its own bindings from the same template.
- Credential-bearing memory and filesystem resume after deleting the install-only
  key. A missing runtime key refuses resume before boot. An injected startup
  failure tears down the resumed VM, NBD and listeners while retaining private
  data; removing the fault permits another resume without repeating install.
- Injected install/startup failures during fresh create: API returns an error,
  no port publication appears, and rollback removes the VM, NBD attachments,
  private storage and instance record. The run exposed a private-storage leak;
  rollback now removes fresh-instance storage after VM teardown, while preserving
  storage on a failed resume. The lease-order regression passes with `-race`.
- Values and the CA private key are absent from all 25 template/snapshot metadata
  files in the test deployment. The fixture HTTPS server validates credentials
  without echoing them; only authentication results and fingerprints are logged.

- Filesystem and memory resume after replacing the orchestrator process with a
  different node ID and an empty cache directory on the same host. Filesystem
  resume retains private storage and install state and reruns startup. Memory
  resume retains the workload PID, shell environment and hook counts. Both
  restore PTY and TCP/UDP publications from durable snapshot storage.
- Concurrent public API creates: independent private volumes, bidirectional
  shared-directory visibility, separate PTYs and noncolliding TCP/UDP listeners.
  Root in one VM is denied a peer private NFS export. Deleting one instance
  leaves the peer's workload, volumes and ports usable.
- Forced worker shutdown with two active guests, including a filesystem-resumed
  instance: guest processes and lifecycle cleanup finish before NFS, networking,
  devices and template resources close. Both runs finish in under ten seconds
  without shutdown errors or VM/NBD/port records left behind. Private storage
  stays available for recovery or explicit deletion. The API may still report
  a stopped instance as running until its timeout expires. With short timeouts,
  expiry removes fresh-instance private data and retains a paused instance's
  data; a replacement worker resumes that snapshot with its original private
  marker, PTY and TCP/UDP ports. Explicit DELETE removes private data and resume
  access. This does not establish forced shutdown during a template build.
- Worker admission refuses one vCPU or 256 MiB for the fixture requiring two
  vCPUs and 512 MiB, before VM boot. Snapshot pin/release/ancestor/pause cache
  tests pass with the race detector.
- Guest runtime HTTPS authenticates while install-only/undeclared SNI and
  plaintext credential traffic are refused. UDP 53/443 probes receive no reply
  from live fixture echo services that answer host control probes.
- Literal-value audit of 35 retained instance snapshots (114 files, 1,096,548,242
  bytes, including disk/memory payloads) finds none of the current synthetic
  credentials or complete private-key PEMs. This does not cover removed key
  versions, encoded fragments or compressed storage backends.

The envd component script is `envd-smoke.py`. Run it inside an isolated container
with `/usr/bin/envd --isnotfc --no-cgroups`, bash, Python and a root-owned mode-0700
`/run/e2b-kit`. It tests no VM behavior.

On the patched KVM deployment, after building the shell template, run:

```sh
python examples/native-kit/native-kit.py smoke TEMPLATE_ID
```

The smoke creates a real sandbox, discovers its native workload, verifies a
terminal using shell input/output, and kills it in a finally block. The separate
local Embed integration runs above exercise failed-init cleanup, credential
injection, install-hook isolation, both resume modes and cross-instance snapshot
rejection. They do not establish distributed placement or cross-worker storage.
Intermediate install snapshots use normal pause storage. In the local file-store
validation, instance DELETE removes API resumability, private volumes, VM/NBD
resources and listeners, but retains snapshot payload directories, including the
install boundary. The file-store deployment has no automatic object collector
configured. Snapshot cache release is distinct from durable object collection;
cache pin/release tests do not establish object reclamation. Do not delete an
install layer while any resumable descendant still references it. A collector
must establish that the instance and all dependent snapshots are no longer live
before removing instance-specific layers. This remains a rollout limitation.


## Additional capability handlers

| Contract | Implemented behavior | Limits |
| --- | --- | --- |
| `agent-skill@1` + `agent-skills@1` | Every bundled directory linked at every discovery path; executable bits and relative references retained; before workload launch | Host sharing off; workload user needs destination access. Required conflicts fail; optional failures recorded. |
| `agent-context@1` | Per-Kit progressive index, staged paths retained, inline files beside every effective profile | Cannot compose with @2, per spec. Existing user sections preserved. |
| `long-running@1` | Detach does not stop the workload | Explicit timeout, kill and lifecycle operations still apply. |
| `resources@1` | Maximum selected CPU/memory requirements; checked at build and create/resume before boot | Whole vCPUs/MiB; binary memory units; GPU unavailable. Explicit allocation must meet requirements. |
| `acp@1` | Complete command, process-local environment, protocol metadata and stdio launcher as workload user | No TTY; stderr remains separate. Agent must appear in composed provides. |

Use `native-kit.py build --cpus 4 --memory-mib 4096 ...` to allocate a template
large enough for its resource declarations. An undersized allocation fails; it
is not silently increased beyond the API's quota accounting.

For ACP, read `$HOME/.local/share/kit-e2b/acp/index.json` as the workload user.
Launch the selected `script` using E2B's ordinary command API with `stdin=True`,
the indexed workload `user`, and separate stdout/stderr callbacks. Feed ACP JSON-RPC
through command stdin. Do not use the PTY attach command for ACP.

Seven contracts still have no handler: `network-policy@2`, `git-identity@1`,
`ssh-agent@1`, `host-port@1`, `privileged@1`,
`usb-device@1`, and `kit-registry@1`. Required declarations fail; optional ones
are skipped by selection. `volume@1` and `host-mount@1` require the configured
native storage backend described above. The volume compatibility skip remains
available explicitly.
`agent-sessions@1` and `agent-interactive-sessions@1` are accepted by default as
metadata, with no runtime session-management operations.
This is 14 implemented contract types, including the restricted network and
credential handlers; it is not a runtime TCK conformance claim.

## Reproduce the Firecracker component test

With Docker, Python 3, `mkfs.ext4`, sudo, and usable `/dev/kvm` on Linux:

```sh
# Supply architecture-matching Firecracker and an uncompressed guest kernel.
FIRECRACKER=/path/to/firecracker KERNEL=/path/to/vmlinux \
  ENVD="$PWD/examples/native-kit/bin/arm64/envd" \
  examples/native-kit/firecracker-smoke.sh
```

The script exports a disposable Python image, injects patched envd, boots a real
VM, and runs the file/process RPC smoke inside it. It checks a completion marker
and rejects kernel panics, then removes the temporary image. `BOOT_LOG` selects
the retained host log path. No host network or storage configuration is changed.
The smoke disables envd cgroups; it does not exercise E2B's NBD, userfaultfd,
snapshot, networking or control-plane paths.

Optionally build a static adapter test binary with
`CGO_ENABLED=0 GOWORK=off go -C ../kit-e2b test -c -o /tmp/adapter.test .`
and set `ADAPTER_TEST=/tmp/adapter.test` to also run the generated ACP launcher,
context attribution and resource planner tests as the unprivileged guest user.

## Recreate a native Kit instance

Build an accessible pristine replacement template, then run:

```sh
python3 examples/native-kit/native-kit.py recreate "$SANDBOX_ID" "$TEMPLATE_ID"
```

This pauses the running VM and recreates it under the same sandbox ID with a
fresh writable filesystem. Declared private volumes retain their data, ownership
and file permissions; omitted paths remain allocated for later compositions.
The retained request settings must match, including unspecified values and tmpfs.
Install/startup hooks run again and tmpfs starts empty. Explicit instance deletion
removes retained private backing storage; shared host directories survive.

The underlying call is `POST /sandboxes/{id}/resume` with
`{"kitTemplateID":"<pristine-template-id>","timeout":900}` on a paused native
instance. The API and worker must mount the same `NATIVE_KIT_STORAGE_ROOT`.
Do not combine this field with `memory`. If a build, admission or boot fails
after pause, the instance remains paused with its previous recovery snapshot;
an ordinary resume recovers that snapshot. Volume request conflicts do not delete
data. This local prototype does not provide automatic rollback of writes a failed
replacement's hooks made to persistent volumes.
