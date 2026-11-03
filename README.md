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
* **Output** is a list of files, each tagged with what a change to it needs:
  * **Registries** (`/etc/containerd/certs.d/*`): applied live.
  * **Kubelet CA** (`/etc/kubernetes/kubelet-ca.crt`): applied live.
  * **Trust bundle** (`/var/ssl/certs/amco-additional-trust-bundle.pem`):
    containerd is restarted.
  * **containerd drop-in** (`/etc/containerd/conf.d/50-amco.toml`):
    containerd is restarted.
  * **Kubelet config** (`/etc/kubernetes/kubelet.conf`): the node is
    drained, then the kubelet is restarted.

  These follow the MCO's defaults for the RHCOS equivalents. Nothing needs a
  reboot.
* **Status** uses the MachineConfigPool vocabulary:
  * Conditions: `Updated`, `Updating`, `Degraded`, `RenderDegraded` and
    `NodeDegraded`.
  * Machine counts.
  * A `warnings` list naming the inputs that cannot take effect on AIX, for
    example CRI-O-only fields.

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

Existing files at managed paths are backed up as `<path>.amco-orig` and put
back when AMCO stops managing the path.

### Drift

The agent checks the managed files every minute. If a file was changed by
hand, the node reports `Degraded`. The agent does not overwrite the change,
which is the MCO's policy too. To recover, do one of these:

* Restore the file.
* Wait for the next rendered config, which rewrites every file.
* Run `touch /etc/amco/force`. The agent then re-applies everything and
  restarts the affected services. That restart happens without a drain, so
  pods on the node see the kubelet restart.

If an apply fails partway (for example a service does not come back), the
agent re-applies the next config in full rather than as a diff.

### Agent safeguards

* The agent only writes the paths listed above. It only accepts a rendered
  ConfigMap that is immutable, labelled with its pool, and whose content
  hashes to its name.
* A node is only updated once its agent has reported in (the `agentVersion`
  annotation). Until then the pool lists it under `warnings`, so a node
  whose agent is not installed yet is never cordoned.
* The agent's token can patch any Node object, and RBAC cannot narrow that.
  `manifests/06-agent-admission-policy.yaml` is a ValidatingAdmissionPolicy
  that limits the token to its own reporting annotations: no spec, label or
  other annotation changes. All nodes share one token; issue one per node if
  one node must not be able to write another's annotations.

## Testing without a cluster or kubelet

Render from manifests, then apply on a scratch LPAR, or anywhere under a
scratch root:

```
make all
_output/linux/amd64/aix-machine-config-operator render --dir examples/ -o rendered.json
amco-agent apply -f rendered.json --root /tmp/amco-root --no-restart   # dry run, any OS
amco-agent apply -f rendered.json                                      # on an AIX test LPAR
```

`render` takes a real `ControllerConfig`:

```
oc get controllerconfig machine-config-controller -o yaml
```

It also takes any IDMS, ITMS, Image, KubeletConfig and ContainerRuntimeConfig
YAML, and prints warnings for anything AIX cannot honour. If a service is not
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
