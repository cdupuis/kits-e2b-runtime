# Native Embed TCK runner

This candidate adapter runs the sandbox TCK from the **pinned Go dependency**
against the patched API, orchestrator and envd. Its output distinguishes failed,
passed and skipped checks. Selecting a type is a test input, not a conformance
claim for the prototype. There are no generated guest probe results: commands
execute through envd with the original argv, while create/stop/start/remove use
the public REST API. Declaration and selection refusal exits with status 2;
registry, storage, build, hook and transport failures remain errors.

## Prepare fixtures

Build on the target Linux architecture with the pinned dependencies and
internal adapter access described in [Sources and build](README.md#sources-and-build):

```sh
NATIVE_KIT_TCK=1 examples/native-kit/build.sh
export NATIVE_TCK_HELPER="$PWD/examples/native-kit/bin/$(go env GOARCH)/native-kit-tck"
"$NATIVE_TCK_HELPER" fixtures /tmp/native-tck-fixtures
"$NATIVE_TCK_HELPER" registry 127.0.0.1:5112
```

Run the remaining commands in another terminal. Provision a local guest base
with Embed's systemd/SSH/NFS prerequisites and Python, curl and git installed.
This avoids guest package downloads when running in a restricted network.
The publisher substitutes this base into the official fixture recipes and
preserves their descriptors, probes and entrypoints. It also stages referenced
content under each Kit's own directory. The catalog must be built from the
helper's extracted fixtures; another spec checkout can have a different catalog.
Use a fresh private state directory after republishing mutable fixture tags or
changing admission semantics; otherwise cached templates retain their old selections.

```sh
python3 examples/native-kit/build-tck-fixtures.py \
  --fixtures /tmp/native-tck-fixtures \
  --helper "$NATIVE_TCK_HELPER" \
  --base-image native-e2b-test:local \
  --catalog /tmp/native-tck-catalog.json
```

The test registry is restricted to loopback publication. Allow ample template
and build-cache space: the preprovisioned shell-derived guest consumes roughly
1.8 GiB per complete base snapshot. Different plans and overrides intentionally
have different template cache keys. Snapshot retention still needs operator
management as described in the main guide. Use a disk-backed
`ORCHESTRATOR_BASE_PATH` for this run: the builder retains several GiB of scratch
files per distinct composition, and a RAM-backed build area can exhaust memory
and produce loop-device write errors even while the durable template store has
free space. Build scratch/cache and retained template snapshots need separate
capacity accounting. Some checks retain six VMs concurrently; provision enough
hugepages for those guests and overlapping template-build VMs. A 2 GiB
hugepage pool is insufficient even when each ordinary fixture uses 512 MiB.

## Run a candidate subset

Start the patched services and their dependencies. The API and orchestrator
must both have permission to manage the configured private storage directory.
Save the local deployment's API key in a mode-0600 file, outside evidence and
source archives. Configure the worker storage/port backends before selecting
their corresponding helper settings:

```sh
export E2B_API_URL=http://127.0.0.1:3000
export NATIVE_TCK_ENVD_URL=http://127.0.0.1:5007
export NATIVE_TCK_CATALOG=/tmp/native-tck-catalog.json
export NATIVE_TCK_API_KEY_FILE=/path/to/private/local-api-key
export NATIVE_TCK_STATE=$(mktemp -d)
export NATIVE_TCK_STORAGE=1
export NATIVE_KIT_STORAGE_ROOT=/path/to/private/shared-native-storage
export NATIVE_TCK_TEAM_ID=<authenticated-local-team-uuid>
export NATIVE_TCK_PORTS=1
export NATIVE_TCK_CLAIMS=com.docker.sandbox/long-running@1
"$NATIVE_TCK_HELPER" run "$PWD/examples/native-kit/native-tck.py" \
  "$NATIVE_TCK_CATALOG" > /tmp/native-tck-report.json
```

`NATIVE_TCK_CLAIMS` is a comma-separated list. Omitted known types are rejected
through the production selector; unknown required types are always rejected.
Start with long-running, then test lifecycle and context separately so failures
remain attributable. Keep the complete JSON report, including skipped checks.

The adapter currently wires generic exec, attachment disconnect, filesystem
pause/resume, status, selection, recreation, retained block-path inspection and
removal, plus create-time argument/env and capability rejection overrides.
Future selection-policy changes apply only at recreation; stop/start keeps the
current immutable template and recorded grants.
Recreation checks the production retained settings before pause and uses the
public resume template override; it never copies guest data or emulates probes.
Supplying replacement references resets Kit arguments to the supplied values;
reference-free recreation keeps existing arguments and applies any overrides.
Run the adapter on the Embed host with access to the private storage registry.
Host-sharing observations use the production storage registry: `host-mounts`
resolves a published Kit repository, `host-mount-read` reads a regular file under
its retained directory, and `host-mount-rm` removes an unreferenced share by its
tenant-scoped handle. These operations continue to work after sandbox deletion.
Reads reject paths and symlinks escaping the share; removal refuses any share
still referenced by a live or paused instance. The fixture publisher gives the
two Kit-update fixtures different tags of the same repository.
The adapter does not wire credential fixtures, host-skill stores
or SSH/git identity fixtures. Tests needing those operations must
remain visible as failures or skips; those capabilities need separate VM evidence
until the adapter is extended. The context versions must be tested together to
exercise mixed-version refusal, and volume must also be selected for the context
selection-boundary test. Select lifecycle@1 alongside volume@1 to
exercise mounts before hooks, as well as persistence and recreation.
Select host-mount@1 with lifecycle@1 and volume@1 to exercise all host-directory
checks, including mounting before hooks and refusing overlapping storage kinds.

Run the adapter's input/transport regressions with:

```sh
python3 examples/native-kit/native-tck-test.py
```

## Local validation, 31 October 2026

On the local ARM64/4 KiB Embed test host, the pinned long-running subset passed
6 applicable checks and the lifecycle subset passed 15. The broader selection
below passed 19, failed 7, warned once and skipped 90 of the 117 checks:

```sh
export NATIVE_TCK_CLAIMS=com.docker.sandbox/agent-context@1,com.docker.sandbox/agent-context@2,com.docker.sandbox/sbx@1,com.docker.sandbox/resources@1,com.docker.sandbox/agent-skill@1,com.docker.sandbox/volume@1
```

All six context checks and the skill-availability check passed. In that run,
seven volume checks failed because recreation and volume-path inspection
were not yet wired. The memory-limit probe reported unknown and warned. These results
establish a tested subset, not full runtime conformance. Hook environment
filtering and required/optional skill admission were corrected during the run;
compiler storage conflicts now use the adapter's explicit refusal status.

Reports and original failed/interrupted attempts are preserved in the project
artifacts `native-e2b-tck-validation.md` and
`native-e2b-tck-validation-logs.zip`, alongside refreshed paired source patches.

## Recreation validation, 1 November 2026

The volume/resources/lifecycle/long-running selection finished with **27 passed,
0 failed, 1 warning and 89 skipped** across 117 checks. All eleven volume checks
passed, including dormant request compatibility, fresh storage before hooks,
recreation persistence, private-path inspection and removal. The lifecycle and
long-running checks in this selection also passed.

The adapter now resets Kit arguments when replacement references are supplied;
reference-free recreation retains them. A direct REST check confirms HTTP 400
for an incompatible retained-volume request and recovery of the original paused
filesystem and volume through ordinary resume. Ordinary create error handling
keeps its prior status mapping.

The memory-limit probe still reports unknown: it reads cgroup limits, while the
VM's physical RAM is bounded by Firecracker. The independent API/guest check
observed a 2048 MiB allocation, 2,030,084 KiB MemTotal and no swap. The unchanged
TCK probe remains a warning. This is selected-capability evidence, not full
runtime conformance. The earlier failed reports are retained in the updated
validation evidence archive.

## Shutdown follow-up, 1 November 2026

After the recreation run, forced-stop cleanup was corrected to release device
slots with an independent deadline and close the observer's meter provider,
including its periodic reader. Disabled telemetry no longer starts an exporter.
Forced stop skips build draining while still closing template resources; a
normal service exit during teardown no longer changes the worker exit status.

The rebuilt worker ran a real 2048 MiB guest, filesystem pause/resume and delete.
The guest reported 2,030,084 KiB MemTotal and zero swap before and after resume;
a file under `/var/tmp` survived. Guest `/tmp` is tmpfs and does not survive that
cold boot. The pinned cgroup probe still returned `unknown`; its earlier TCK
warning is unchanged. These independent observations do not replace TCK results.

Two forced shutdowns and one graceful shutdown finished in under ten seconds
each without shutdown errors. Both replacement workers returned exit status 0.
All instances were removed before shutdown; this does not test forced shutdown
with active guests or builds. Race tests for factories and metrics, and pinned
runtime lint for those packages, pass. Raw logs, the first marker attempt and
cleanup observations are retained in the validation evidence archive.


## Host-directory sharing validation — 1 November 2026

The latest pinned TCK run reports **38 passed, 0 failed, 0 warnings and 79 skipped**, across 117 checks. It selects host-mount, volume, lifecycle and long-running capabilities. This extends the earlier reports; it does not establish full runtime conformance.

| Capability | Passed | Failed | Warnings | Skipped |
| --- | ---: | ---: | ---: | ---: |
| host-mount@1 | 12 | 0 | 0 | 1 |
| volume@1 | 11 | 0 | 0 | 0 |
| lifecycle@1 | 4 | 0 | 0 | 0 |
| long-running@1 | 2 | 0 | 0 | 0 |
| Other checks | 9 | 0 | 0 | 78 |

The host storage library now lists retained shares by tenant and canonical Kit repository, reads bounded regular files within a share, and removes unreferenced shares by their listing handle. Revisions share the same repository identity. Removal serializes with allocation and instance removal and refuses live or paused references. Reads reject escaping paths, escaping symlinks, special files and observations over 1 MiB. The TCK adapter uses these production operations for independent host observations after guest deletion.

The first live run found a refusal-classification defect: the production compiler rejected overlapping host-mount and private-volume declarations, but the helper returned a general error. The helper now recognizes that explicit compiler diagnostic as refusal. Joined I/O errors remain infrastructure failures. The original attempt’s output and direct before/after compiler observations are preserved, alongside the corrected full rerun. The rerun overlapped original report collection on the same worker, with independent sandbox identities and host-directory paths.

The first attempt exited without a JSON report; its raw stderr and empty output are retained. The original helper discarded partial TCK findings on an expired suite context. The direct compiler refusal defect was captured separately, and only the corrected complete report supplies the pass/fail counts above.

The complete live run uses the refusal-corrected helper. A subsequent reporting follow-up raises the suite budget from 90 minutes to three hours and preserves partial JSON with incomplete=true and the run error while still exiting nonzero. Its success, failed-suite and interrupted-report branches have race-enabled regression coverage; the rebuilt helper and pinned lint pass. This follow-up changes reporting and the outer time budget, not guest probes or runtime behavior.

Storage and helper race regressions, Python adapter tests and runtime-pinned lint pass. The live suite uses the original pinned probes and official fixture descriptors. The two Kit-update fixtures publish distinct tags of one repository. All host-directory tests observe real local Firecracker guests and production NFS-backed directories; no probe outcomes are supplied by the adapter.

The test filesystem was expanded online from 350 to 650 GiB to preserve snapshot ancestors while accommodating randomized-path template builds and disk-backed disposable worker scratch. Cleanup removes that scratch, stops services and dependency containers, verifies no VM/NBD/private-instance/port allocations, detaches the loop filesystem, and restores the original hugepage pool. Existing retained shared records and durable snapshot ancestors are preserved. The cleanup log parser initially rejected an invalid UTF-8 byte; shutdown checks use replacement decoding while the original raw log remains unchanged in evidence.

The resource cgroup probe warning from the earlier resource-selected run remains unresolved; resources are not selected in this latest run. Host skill-store controls, credential and SSH/git identity TCK fixtures, distributed placement/storage and durable snapshot garbage collection remain outside this run. The earlier separate VM credential/port/skill evidence retains its stated scope.

The evidence is split across native-e2b-tck-validation-logs.zip (historical runs) and native-e2b-host-sharing-validation-logs.zip (this host-sharing run, its timeout, cleanup and regressions), because the artifact gateway accepts at most 4 MiB per encoded request. Raw logs are preserved.

## Active-guest shutdown follow-up — 1 November 2026

Two real forced shutdowns with two active guests each finish in 9.824 and
9.420 seconds. Each pair contains a fresh instance and a filesystem-resumed
instance, with private NFS storage, interactive PTYs and TCP/UDP publications.
After shutdown, no VM, NBD attachment, listener or publication record remains.
Private markers survive until a control-plane storage decision.

The original run left orphaned VMs, NBD attachments and port records and logged
shutdown failure after 39.279 seconds. Shutdown now stops tracked guests
concurrently and joins their lifecycle cleanup after RPCs finish and before NFS,
networking, device pools or templates close. Regression tests cover overlapping
lifecycles, concurrent stop delivery, cleanup joining, failure propagation and
budget expiry. Full server and factories race suites and pinned lint pass.
The server test binary is compiled in the paired workspace and run with
GOWORK=off so its stand-in envd builds can use their temporary modules.

The API retains a stopped guest's running record until timeout; an immediate
resume can return 409. A second run sets 45-second timeouts through the public
API before shutdown. Expiry removes fresh private storage, changes the resumable
instance to paused and retains its private marker. A replacement worker resumes
that snapshot with its marker, PTY and TCP/UDP publications. Explicit DELETE
removes private storage and resumability. The first recovery observation
incorrectly waited for GET to return 404 for a retained snapshot; the corrected
observation accepts its public paused state. Both observations are retained.

These are additional lifecycle observations, not another TCK run. The latest
pinned TCK counts remain 38 passed, 0 failed, 0 warnings and 79 skipped. The
earlier resource probe warning remains. Active-build shutdown, host skill-store
and credential/SSH/git TCK fixtures, multi-host storage and durable snapshot
collection remain unverified. The new evidence is in
native-e2b-active-shutdown-validation-logs.zip.
