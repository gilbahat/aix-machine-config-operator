# AIX Machine Config Operator (AMCO)

AMCO manages the node configuration of IBM AIX nodes in an OpenShift cluster.
It plays the role that the machine-config-operator (MCO) plays for RHCOS
nodes, and is built the way the
[Windows Machine Config Operator](https://github.com/openshift/windows-machine-config-operator)
is: a separate operator that the stock MCO never interferes with, managing
bring-your-own-host (BYOH) nodes.

The AIX node stack it configures is
[containerd-aix](https://github.com/reuna-labs/containerd-aix) and the coming
AIX kubelet port. Other pieces of the stack (TopoLVM, Solo5 WPAR isolation,
buildah, trivy) are independent of AMCO.

On top of that Kubernetes plumbing, `AIXMachineConfig` lets you declare host
configuration: files, SRC subsystems, inittab entries, kernel tunables and
device attributes. The aim is to replace Ansible and ad-hoc tooling for AIX
nodes once they have joined. AIX nodes are mutable and long-lived, so AMCO
converges them in place, item by item, and **never reboots a node**: see
[docs/disruption-model.md](docs/disruption-model.md).

> Status: early. The operator and the agent are covered by unit tests and an
> offline render-and-apply test, and the agent cross-compiles for AIX. Neither
> has run against a live cluster or on AIX hardware yet, and the kubelet port
> it writes configuration for does not exist yet.

## How it works

```
 cluster objects (read only)                AMCO operator (Deployment, Linux)               AIX node
 ─────────────────────────────              ──────────────────────────────────             ─────────────────────────────
 ControllerConfig (CA bundles,  ─┐           1. label kubernetes.io/os=aix nodes            amco-agent (SRC subsystem)
   cluster DNS IP)               │              node-role.kubernetes.io/aix                  - watches its Node
 Image, IDMS, ITMS              ─┼─ render ─▶ 2. ConfigMap rendered-<pool>-<hash>   ──get──▶  - writes files atomically
 ContainerRuntimeConfig         ─┤              (immutable, content addressed)               - restarts containerd /
 KubeletConfig, APIServer       ─┘           3. per node, within maxUnavailable:               kubelet through SRC
                                                cordon + drain (if needed)                   - reports currentConfig,
 AIXConfigPool (amco.reuna-labs.io) ◀─status─   set desiredConfig annotation  ──annotate──▶    state, reason
                                             4. uncordon when the agent reports Done ◀───────
```

* **Inputs** are the objects the MCO already renders RHCOS configuration from,
  so cluster administrators configure AIX nodes with the same objects:
  * The `ControllerConfig` provides the cluster's CA bundles, which the MCO
    keeps current: the kubelet client CA, the additional trust bundle and the
    per-registry CAs.
  * The `Image` config, `ImageDigestMirrorSet` and `ImageTagMirrorSet` are
    rendered into containerd `hosts.toml` files.
  * `ContainerRuntimeConfig` and `KubeletConfig` select pools with the same
    `machineConfigPoolSelector`, matched against `AIXConfigPool` labels.
* The pool's `configSelector` picks `AIXMachineConfig`s, merged in name
  order (see [Host configuration](#host-configuration-aixmachineconfig)).
* **Output** is one rendered config per pool: AMCO's own files plus the host
  items, each with what a change to it needs:
  * **Registries** (`/etc/containerd/certs.d/*`): applied live.
  * **Kubelet CA** (`/etc/kubernetes/kubelet-ca.crt`): applied live.
  * **Trust bundle** (`/var/ssl/certs/amco-additional-trust-bundle.pem`):
    containerd is restarted, without a drain (shim v2 containers survive a
    containerd restart).
  * **containerd drop-in** (`/etc/containerd/conf.d/50-amco.toml`):
    containerd is restarted, without a drain.
  * **Kubelet config** (`/etc/kubernetes/kubelet.conf`): the node is
    drained, then the kubelet is restarted, as the MCO does.
  * **Host items**: whatever the item says (below). Nothing reboots.
* **Status** uses the MachineConfigPool vocabulary:
  * Conditions: `Updated`, `Updating`, `Degraded`, `RenderDegraded` and
    `NodeDegraded`.
  * Machine counts, including nodes with changes pending a reboot and nodes
    with items that need reprovisioning.
  * A `warnings` list naming the inputs that cannot take effect on AIX (for
    example CRI-O-only fields) and every item one `AIXMachineConfig`
    overrides in another.
* **Node conditions**, set by the operator from the agent's reports:
  `AIXRebootRequired`, `AIXReprovisionRequired` and `AIXConfigDrift`.
  `AIXKernelUpdatePending` is reserved for coordinated kernel updates.

### Why the stock MCO leaves AIX nodes alone

* The MCD DaemonSet only schedules on `kubernetes.io/os: linux`.
* AIX nodes get `node-role.kubernetes.io/aix` instead of
  `node-role.kubernetes.io/worker`, so the MCO's worker pool does not select
  them. AMCO adds this label because the kubelet cannot set node-role labels
  itself.
* AMCO uses its own `AIXConfigPool` resource and its own
  `amco.reuna-labs.io/*` annotations. It never creates MachineConfigs or
  MachineConfigPools.
* If someone adds the worker label to an AIX node anyway, the pool reports a
  warning. The MCO would otherwise wait forever for a daemon that never runs.
  A small MCO patch makes MCO skip every non-Linux node, as it already does
  for Windows nodes. It is on the `non-linux-node-exclusion` branch of
  [gilbahat/machine-config-operator](https://github.com/gilbahat/machine-config-operator)
  and is not needed for AMCO to work.

### Node annotations

| Annotation (`amco.reuna-labs.io/…`) | Written by | Meaning |
|---|---|---|
| `desiredConfig` | operator | rendered config the node should run |
| `currentConfig` | agent | rendered config the node runs |
| `state` | agent | `Working`, `Done` or `Degraded` |
| `reason` | agent | error behind `Degraded` |
| `cordonedFor` | operator | AMCO cordoned the node for this config and will uncordon it |
| `agentVersion`, `osLevel` | agent | reported at start-up |
| `bootTime` | agent | when the node booted (uptime is the time since) |
| `pendingReboot` | agent | JSON list of items that take effect at the next boot |
| `reprovisionRequired` | agent | JSON list of `{item, reason}` that cannot converge in place |
| `drift` | agent | JSON list of `{item, reason}` whose live state no longer matches |
| `bosboot` | agent | `{state: Rebuilt\|Failed, time, error}` for a bosboot since the last boot |

## Host configuration (AIXMachineConfig)

```yaml
apiVersion: amco.reuna-labs.io/v1alpha1
kind: AIXMachineConfig
metadata:
  name: 60-network-tunables
  labels:
    amco.reuna-labs.io/role: aix      # the default pool selects this label
spec:
  tunables:
  - {command: "no", name: tcp_sendspace, value: 262144}
```

More in [examples/machineconfigs.yaml](examples/machineconfigs.yaml): an
sshd_config with an sshd refresh, network tunables, FC adapter queue depth,
and an application subsystem started from one inittab entry.

| Section | Fields | Applied with |
|---|---|---|
| `files` | `path`, `contents.inline` or `contents.base64`, `mode` (default 0644), `owner`/`group` (name or id), `onChange` (`Restart`/`Refresh` a subsystem) | atomic write, chown, chmod |
| `subsystems` | `name`, `state` (`Active`/`Inoperative`), optional `definition` (path, arguments, uid, stdin/out/err, respawn, multipleInstances, contact, signals, waitSeconds, group) | `startsrc`/`stopsrc`, `mkssys`/`chssys`/`rmssys` |
| `inittab` | `identifier`, `runLevel`, `action`, `command` | `lsitab`/`mkitab`/`chitab`/`rmitab`; no other entry is touched |
| `tunables` | `command` (`no`, `vmo`, `ioo`, `schedo`, `nfso`, `raso`), `name`, `value` | `-p` or `-r`, by the tunable's type |
| `deviceAttributes` | `device`, `attribute`, `value`, `onBusy` (`Defer`/`Fail`) | `chdev`, or `chdev -P` when busy |
| `kernelUpdate` | reserved | rejected: not yet supported |

Every item also takes `drain` (cordon and drain before it changes; default
false) and `required` (see below).

* **Merging.** Configs merge in name order. For the same item (a file path,
  subsystem name, inittab identifier, tunable, or device attribute) the later
  config wins, and the pool lists each override as a warning.
* **Convergence.** For each item the agent reads the live state, changes only
  what differs and verifies the result. Re-applying an applied config runs
  no command that changes anything. Retrying a failed apply only runs what
  is still owed: services whose restart already succeeded are not restarted
  again.
* **Outcomes.** Each item ends `Live` (applied and verified),
  `ServiceRestart` (applied, then a subsystem restarted or refreshed),
  `PendingReboot` (applied persistently, effective at the next boot) or
  `ReprovisionRequired` (cannot converge in place; skipped and reported,
  unless it is `required`, in which case nothing is changed and the node is
  `Degraded`).
* **Tunables by type** (the TYPE letter from the command's `-x` listing):

  | Type | Action | Outcome |
  |---|---|---|
  | D Dynamic, C Connect, M Mount | `-p` (now and next boot) | Live (C and M affect new connections and mounts) |
  | I Incremental | `-p` to raise; `-r` to lower | Live, or PendingReboot |
  | R Reboot | `-r` | PendingReboot |
  | B Bosboot | `-r`, then `bosboot -ad /dev/ipldevice` | PendingReboot; a failed bosboot makes the node Degraded and is not retried automatically |
  | S Static, d deprecated, other | none | Degraded |

  Restricted tunables (listed only with `-F`) are refused: the commands ask
  for confirmation to change them and IBM advises against it.
* **Devices.** A live `chdev` is tried first. If the device is busy,
  `onBusy: Defer` (default) records the change with `chdev -P` as
  PendingReboot; `Fail` makes the node Degraded. A missing device or an
  attribute that is not user-settable is ReprovisionRequired.
* **Subsystems.** A subsystem with no definition that is not defined on the
  node is ReprovisionRequired. Changing the definition of a running
  subsystem restarts it. `onChange` actions only run on active subsystems
  and are skipped for subsystems just started.
* **Baselines and removal.** Before AMCO first manages an item, the agent
  records its original state: a file's content (kept under
  `/etc/amco/backups/<path>`), mode and owner, or its absence; the subsystem
  state, and its definition if the item manages it; the inittab entry; the
  tunable or attribute value. When the item leaves the merged config, even
  one that never finished applying, it is restored by the same rules as
  applying, so restoring a Reboot tunable is itself PendingReboot. An item
  whose original state AMCO could not write back (for example an empty
  device attribute) is not managed at all and is reported as
  ReprovisionRequired; a restore that fails is reported as drift and given
  up rather than blocking later configs.
* **After a reboot**, whoever performed it, the agent compares the boot time
  with the boot during which each pending item was set, and clears the items
  whose boot has happened. The next drift check reports any that are still
  not effective. The boot time is derived from init's uptime
  (`ps -o etime= -p 1`) rather than `who -b`, which has no year and so is
  ambiguous on LPARs up for over a year.
* **Reserved.** Paths AMCO renders itself, `/etc/amco`, `/opt/amco`,
  `/etc/tunables` and `/etc/inittab`, the `amco-agent` subsystem and inittab
  entry. Names (devices, attributes, tunables, subsystems, inittab
  identifiers, owners) are checked against strict patterns in the CRD
  schema, by the operator and again by the agent, and the agent never runs a
  shell: every command is a fixed absolute path with an argument array.

**Privilege.** An `AIXMachineConfig` runs as root on every node it reaches:
subsystem definitions and inittab entries start arbitrary programs. Writing
one is equivalent to cluster-admin, as it is for a MachineConfig.
`manifests/08-machineconfig-rbac.yaml` ships an editor role that is not
aggregated into `admin` or `edit`; bind it only to whoever you would give
cluster-admin.

## Relationship to the MCO code

* AMCO imports `github.com/openshift/machine-config-operator` and
  `github.com/openshift/api` as ordinary module dependencies, pinned to the
  OpenShift `release-5.0` branch (`go.mod`). Its `replace` block mirrors the
  MCO's, as WMCO's does.
* No MCO file is copied or patched. What AMCO uses from the MCO:
  * the exported helpers `kubeletconfig.DecodeKubeletConfig`,
    `kubeletconfig.EncodeKubeletConfig`,
    `ctrlcommon.GetSecurityProfileCiphers*` and `ctrlcommon.ControllerConfigName`;
  * `runtime-utils`' `registries.EditRegistriesConfig`, which the MCO also
    uses for `registries.conf`.
* Three helpers are unexported in the MCO, so they are copied into
  `pkg/upstream/` with attribution to the source commit:
  `validateRegistriesConfScopes`, `convertICSPToIDMS` and
  `validateUserKubeletConfig`. `hack/check-upstream-copies.sh` diffs them
  against the pinned MCO.

The agent deliberately imports none of this. It depends only on client-go and
this repository, so it builds for `GOOS=aix` without patching any vendored
code.

## Install

1. **Operator.** Build the image with `make image`, then set it in
   `manifests/04-deployment.yaml` and apply the manifests:

   ```
   oc apply -f manifests/
   ```

2. **Agent credentials.** Create the agent's kubeconfig (it holds the
   `amco-agent` service account token):

   ```
   hack/agent-kubeconfig.sh > kubeconfig
   ```

3. **Agent.** Build it with `make agent-aix`. Then, as root on each AIX node:

   ```
   ./install-agent.sh -b ./amco-agent -k ./kubeconfig
   ```

   This registers the SRC subsystem `amco-agent` in group `kubernetes`. It
   starts at boot from `/etc/inittab`, after TCP/IP, and logs to
   `/var/log/amco-agent.log`.

4. **Node prerequisites.**
   * containerd-aix runs as the SRC subsystem `containerd`, and the kubelet as
     `kubelet`. Both names are in `pkg/rendered`.
   * `/etc/containerd/config.toml` imports `/etc/containerd/conf.d/*.toml`.
   * The kubelet is started with `--config /etc/kubernetes/kubelet.conf`.

Existing files at managed paths are backed up under `/etc/amco/backups` and
put back when AMCO stops managing the path. Keeping backups out of the
original directory means nothing that scans it (an `rc.d` `S*` script, a
`conf.d` drop-in) picks up the old copy.

### Drift

Every minute the agent compares the live state of every managed item (file
content, mode and owner; subsystem state and definition; inittab entry;
tunable value; device attribute) with the applied config. Drift is
reported, never repaired: the node reports `Degraded`, the `drift`
annotation lists the items, and the operator sets `AIXConfigDrift`. To
recover, do one of these:

* Restore the item.
* Wait for the next rendered config, which re-applies everything in full.
* Run `touch /etc/amco/force`. The agent then re-applies everything and
  runs every item's subsystem actions. That restart happens without a drain,
  so pods on the node see a kubelet restart. The force file also retries a
  failed bosboot.

Every apply is recorded as in progress before the first change, together
with each item's baseline. Each change is preceded by a save of what it will
owe: the subsystem restarts or refreshes, a pending-reboot entry, a needed
bosboot. An apply that fails or is interrupted (agent killed, service not
coming back) is simply run again: every step checks the live state first,
owed actions are still owed, items of the unfinished config are still known
(and restored if the next config drops them), and a subsystem that AMCO
stopped for a restart is started again on every run until it is up. A
failed bosboot is the exception: it is only retried by the force file or a
new config.

### Agent safeguards

* AMCO's own files only go to the paths listed above, and user files never
  to reserved paths. The agent only accepts a rendered ConfigMap that is
  immutable, labelled with its pool, and whose content hashes to its name.
* A node is only updated once its agent has reported in (the `agentVersion`
  annotation). Until then the pool lists it under `warnings`, so a node
  whose agent is not installed yet is never cordoned.
* The agent's token can patch any Node object, and RBAC cannot narrow that.
  `manifests/06-agent-admission-policy.yaml` is a ValidatingAdmissionPolicy
  that limits the token to its own reporting annotations (including the
  pendingReboot, reprovisionRequired, drift, bootTime and bosboot reports):
  no spec, label or other annotation changes. The agent cannot set node
  conditions; the operator does. All nodes share one token; issue one per node if
  one node must not be able to write another's annotations.

## Testing without a cluster or kubelet

Render from manifests, then apply on a scratch LPAR, or anywhere under a
scratch root:

```
make all
_output/linux/amd64/aix-machine-config-operator render --dir examples/ -o rendered.json
amco-agent apply -f rendered.json --root /tmp/amco-root --no-restart   # dry run, any OS: files only
amco-agent apply -f rendered.json                                      # on an AIX test LPAR
```

`render` takes a real `ControllerConfig`:

```
oc get controllerconfig machine-config-controller -o yaml
```

It also takes any IDMS, ITMS, Image, KubeletConfig, ContainerRuntimeConfig
and AIXMachineConfig YAML, and prints warnings for anything AIX cannot honour
and for every override between configs. Off AIX, `apply` is a dry run: it
writes files under `--root` without ownership and skips every other item. If a service is not
registered with SRC (for example, the kubelet before its port lands), `apply`
skips that restart with a warning.

To exercise the cluster path before the kubelet exists:

1. Create a Node object by hand, labelled `kubernetes.io/os: aix`, with a
   `Ready` condition.
2. Run the agent with `--node-name` set to that node's name.

## Known gaps

containerd's `hosts.toml` is less expressive than `registries.conf`. Each of
these limits shows up as a pool warning:

* `blockedRegistries` and `allowedRegistries` are not enforced.
* Search registries are not supported.
* Wildcard scopes are not supported.
* A mirror can only add a path prefix, not rename a repository.
* Mirrors are per host: a mirror configured for one repository is also
  tried for every other repository on that registry (images it does not
  have fall back to the registry).
* `NeverContactSource` can only be enforced for whole-registry sources.
* Tag mirrors (ITMS) are also used for digest pulls.
* Insecure registries are written as `http://` hosts with `skip_verify`, so
  containerd tries HTTPS without verification first and then plain HTTP.

Other gaps:

* `KubeletConfig.logLevel` and `autoSizingReserved` are ignored.
* `KubeletConfig` fields that need cgroups are rejected.
* If a `KubeletConfig` or `ContainerRuntimeConfig` selects only AIX pools, the
  stock MCO marks it `Failure: could not find any MachineConfigPool`. AMCO
  still applies it.
* No CSR approval for AIX kubelets yet. WMCO approves CSRs for its BYOH nodes;
  AMCO will need the same once the kubelet port exists.
* The kubelet kubeconfig and its CA are not rotated. Kubelet feature gates
  are not rendered.
* AIX nodes don't get the `/etc/hosts` entry for
  `image-registry.openshift-image-registry.svc` that the Linux-only
  node-resolver DaemonSet writes on RHCOS. The hosts.toml for the internal
  registry is rendered, but the name must resolve some other way.
* Host configuration does not cover users and SSH keys, filesets and RPMs,
  ifixes, network interfaces or kernel updates yet. `kernelUpdate` is
  reserved in the API for Live Kernel Update, coordinated by the operator.
* File contents are not secret: every selected `AIXMachineConfig` is copied
  into the pool's rendered ConfigMap, readable by anyone who can read
  ConfigMaps in the operator namespace and by the shared agent token. Do not
  put secrets in configs.
* Owner and group names are resolved from the local `/etc/passwd` and
  `/etc/group` (Go's pure `os/user`); users that only exist in LDAP need a
  numeric id.
* After `chdev -P` the ODM already shows the new value, so after a reboot a
  deferred device attribute is checked against the ODM, not against the
  running device.

## Verify on hardware

Nothing has run on AIX yet. The parsers are unit-tested against sample
output written from IBM documentation and published examples; these are the
formats and behaviours to confirm on a real LPAR first:

* **Command paths.** `/usr/bin/{lssrc,startsrc,stopsrc,refresh,mkssys,chssys,rmssys,ps,oslevel}`,
  `/usr/sbin/{lsitab,mkitab,chitab,rmitab,lsdev,lsattr,chdev,bosboot}` and
  `/usr/sbin/{no,vmo,ioo,schedo,nfso,raso}`.
* **`ps -o etime= -p 1`** prints init's elapsed time as `[[dd-]hh:]mm:ss`.
* **`lssrc -S -s <name>`** prints a `#`-prefixed header and a colon-separated
  row, with action, multi and contact as `-R`/`-O`, `-Q`/`-q`, `-S`/`-K`
  (confirmed from one published sample). Arguments containing `:` are
  rejected because they would break this format.
* **`chssys -a ""`** clears arguments and **`chssys -G ""`** removes the group.
* **`lsitab <id>`** exits non-zero with no output for a missing entry; it
  prints `id:runlevel:action:command`. mkitab and chitab take effect without
  `telinit q`.
* **Tunable `-x` listings**: `tunable,current,default,reboot,min,max,unit,type,{dtunable }`,
  with values abbreviated as `256K`, `2G-1`, `8E-1`; `-x` alone omits
  restricted tunables and `-F -x` lists them. Type letters D, S, R, B, M, I,
  C, d as above.
* **`<cmd> -r -o name=value`** for a Bosboot tunable prompts to run bosboot
  and accepts `n` on stdin; `-r` on a Reboot tunable does not prompt.
* **`no -p`** on Connect tunables restarts inetd (documented behaviour).
* **`lsattr -El <dev> -a <attr> -F value:user_settable`** prints
  `value:True`; it exits 255 for an unknown attribute. `lsdev -C -l <dev>`
  prints nothing for an unknown device.
* **`chdev`** on a busy device fails with `0514-062`; `chdev -P` succeeds and
  the new value shows in `lsattr -El` straight away.
* **`bosboot -ad /dev/ipldevice`** exit status reflects failure.
* `refresh -s sshd` fails for signal-only subsystems, so the agent falls
  back to SIGHUP (sshd's documented reload).

## Building

`make all` builds both binaries:

* `_output/linux/<arch>/aix-machine-config-operator`
* `_output/aix/ppc64/amco-agent`

`make verify` runs vet, the tests and gofmt. It also checks that the agent
pulls in no MCO or containers/image code.

`vendor/` holds exactly the packages the module imports, at the versions the
pinned MCO requires. `go.sum` carries the upstream checksums. After moving the
pin, run:

```
go mod tidy && go mod vendor && hack/check-upstream-copies.sh
```

## License

Apache License 2.0, like the MCO.
