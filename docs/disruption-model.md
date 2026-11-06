# Disruption model: in place, no self-reboot

## Decision

AMCO converges AIX nodes **in place**, item by item, and **never reboots or
reprovisions a node itself**.

* For each item the agent reads the live state, changes only what differs
  and verifies the result, as an Ansible module does.
* Changes that only take effect at the next boot are applied persistently,
  recorded as pending, and reported (`AIXRebootRequired`).
* Changes that cannot converge in place are reported as needing
  reprovisioning (`AIXReprovisionRequired`) and skipped.
* Reprovisioning belongs to the LPAR layer: a separate controller, or the
  site's existing processes. It never belongs to AMCO.

The one planned exception is specific kernel updates. They will prefer Live
Kernel Update and be coordinated by the operator. Only the API slot
(`AIXMachineConfig.spec.kernelUpdate`, rejected for now) and the condition
name (`AIXKernelUpdatePending`) exist today.

## Why

* **AIX nodes are pets with long lives.** An AIX LPAR typically carries
  years of state, SAN-attached LUNs and site-specific tuning. It is not
  rebuilt from an image the way an RHCOS node is. The MCO's model of
  rendering a whole OS image and rebooting into it does not fit. What AMCO
  must replace is Ansible-style convergence.
* **Reboots are expensive and owned elsewhere.** An LPAR reboot takes
  minutes to tens of minutes, can involve the HMC, VIOS and SAN paths, and
  is scheduled in maintenance windows by people or tooling that know about
  workloads AMCO cannot see (databases, clusters, licensing). A node that
  reboots itself because a config changed would take that decision away from
  them.
* **Reporting serves the decision.** Rather than rebooting, AMCO tells
  operations which nodes carry changes into their next boot (pending items,
  whether the boot image was rebuilt) and how long each node has been up.
  That shows where a maintenance window carries the most risk.
* **Not every change can converge in place**, and pretending otherwise is
  worse than saying so. A missing adapter, a missing subsystem or an
  attribute that is not user-settable cannot be fixed by AMCO. These belong
  to whoever provisions the LPAR, so AMCO reports them and moves on. An item
  marked `required` blocks the config instead.

## Outcomes

| Outcome | Meaning | Examples |
|---|---|---|
| Live | applied and verified | file written; Dynamic, Connect or Mount tunable set with `-p`; device attribute changed with `chdev`; inittab entry; subsystem started or stopped |
| ServiceRestart | applied, then an SRC subsystem restarted or refreshed; drains only if the item says `drain: true` (and for AMCO's own kubelet config, as in the MCO) | sshd_config + refresh; containerd drop-in + containerd restart (shim v2 containers survive it); kubelet.conf + drain + kubelet restart; a changed subsystem definition |
| PendingReboot | applied persistently, takes effect at the next boot, whoever performs it | Reboot tunable set with `-r`; Bosboot tunable with `-r` and `bosboot`; lowering an Incremental tunable; `chdev -P` on a busy device |
| ReprovisionRequired | cannot converge in place; skipped and reported (Degraded instead if `required`) | device not present; attribute not user-settable; subsystem not defined and no definition given |

Errors are not outcomes; they make the node `Degraded`. Examples are a
Static, deprecated or restricted tunable, an unknown tunable or attribute,
and a failed bosboot (not retried automatically).

## Consequences

* The code base has no reboot or shutdown path, and none may be added
  outside the future kernel-update work.
* Pending items are cleared after a reboot by comparing boot times, so they
  clear no matter who rebooted. An item still not effective after the boot
  is reported as drift.
* Removing an item restores its recorded baseline by the same rules. So
  undoing a Reboot tunable is itself PendingReboot, and a removal never
  needs a reboot either.
* Rollouts keep `maxUnavailable`, pause and drain semantics. The drain
  decision is per item.
