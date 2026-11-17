<p align="center">
  <img src="kumaco.svg" alt="kumaco logo" width="200">
</p>

# kumaco - Kubernetes Maintenance Controller

- [Introduction](#introduction)
- [Behaviour](#behaviour)
  - [The personas](#the-personas)
  - [The kumaco.kured.dev MaintenanceWindow CRD](#the-kumacokureddev-maintenancewindow-crd)
  - [What does it reconcile, and when?](#what-does-it-reconcile-and-when)
  - [What does it look like on my node?](#what-does-it-look-like-on-my-node)
  - [Heartbeat contract](#heartbeat-contract)
  - [Condition types and their owners](#condition-types-and-their-owners)
  - [How does it work?](#how-does-it-work)
  - [Handling MaintenanceWindow conflicts](#handling-maintenancewindow-conflicts)
  - [Multi-tenancy while doing self-service](#multi-tenancy-while-doing-self-service-is-not-an-afterthought)
  - [How kured relies on kumaco](#how-kured-relies-on-kumaco-and-how-to-configure-your-maintenance-window-for-kured)
  - [Using kumaco with the node readiness controller](#using-kumaco-with-the-node-readiness-controller)
  - [Using kumaco with node problem detector](#using-kumaco-with-node-problem-detector)
- [Metrics](#metrics)
- [Gotchas](#gotchas)
- [Debugging](#debugging-with-kumaco-events)
- [Security and access rights](#security-and-access-rights)
- [Getting Help](#getting-help)
- [Trademarks](#trademarks)

This code is in early alpha.

## Introduction

kumaco (KUbernetes MAintenance COntroller) is a kubernetes controller
watching a "MaintenanceWindow" Custom Resource (declaring maintenance windows)
and sets nodes matching criteria under _maintenance_ (by applying the
Kubernetes 1.37 standardized `MaintenanceInProgress` node condition, on the appropriate nodes).
See [Kubernetes v1.37: Introducing Node Lifecycle Conditions](https://kubernetes.io/blog/2026/09/09/kubernetes-v1-37-node-lifecycle-conditions/).

kumaco is the orchestration core of the kubereboot (kured) project.
Yet, kumaco has a purpose outside kured:
- Set one or multiple nodes under maintenance at specific time based on the node-problem-detector information;
- Set one or multiple nodes under maintenance when security patches need to be applied;
- Set one or multiple nodes under maintenance when firmware need changes;
- Set one or multiple nodes under maintenance when multiple factors are met;
- Remove one or multiple nodes from maintenance when maintenance window is over;
- Remove one or multiple nodes from maintenance when conditions are not met anymore;
- Send events to admins when node conditions are met.

In kured v2 architecture:
- kumaco is the _brain of the orchestration_ 
- [node-reboot-required-reporter](https://github.com/kubereboot/node-reboot-required-reporter) detects whether a reboot is required and mark the node as such (it is running on all nodes).
- [reboot-inhibitors-reporter](https://github.com/kubereboot/reboot-inhibitors-reporter) detects whether a blocker pod is present on nodes or a prometheus alert is present (this is not running on all nodes)
- [kured](https://github.com/kubereboot/kured) drains and restarts the nodes when the condition of its kind of maintenance (see `conditionType`) is `True` (it is running on all nodes, but with different privileges as node-reboot-required-reporter)

## Behaviour

### The personas

Two kind of people are involved in kumaco's deployment:
- kumaco's deployer, who has admin rights on the cluster. It deploys the kumaco controller (CRD, deployment, service account, service)
- kumaco's maintenance manager, who has administrative rights to put a node in/out of maintenance, but has at most read access to the nodes themselves.

### The kumaco.kured.dev MaintenanceWindow CRD

A CRD allow us to:
- Validate input easily like any config map would
- Report the state of the MaintenanceWindow (`InProgress`, `Scheduled`, `Closed`, `Invalid`)
- Use RBAC to grant access to different people (a "maintenance manager" might have access to define the conditions/timing of a maintenance window, but might not have write access to the node)
- Let people define their own conditions to start a maintenance.

The MaintenanceWindow CRD's spec contains:
- `startTime`: a cron-like string.
  It is structured as the following:
  - an optional `CRON_TZ=<IANA timezone>`. Without `CRON_TZ`, the timezone is UTC.
  - seven Quartz-style fields: second, minute, hour, day of month, month, day of week, year.
  
  For example,
  `CRON_TZ=Europe/Brussels 0 0 2 1 1 ? 2027` starts once at 02:00 Brussels time on 2027-01-01;
  `CRON_TZ=Europe/Brussels 0 0 2 * * ? *` starts daily at 02:00 local time.

  The timezone prefix plus year-aware fields is kumaco's defined input format, not a
  universally standardized cron expression. No common cron library parses it, so kumaco
  needs its own parser. Because a window is active when `now` falls in
  `[start, start+duration)`, it must also find the latest occurrence at or before `now`
  (all of them, if occurrences overlap), not only the next one.

- `duration` (duration): maximum real elapsed time for a maintenance window. In other words: a maintenance window is active if between `[startTime, startTime+duration)`
- `nodeSelector` (kubernetes labelSelector): the nodes on which the maintenance window is applied to.
  A node under maintenance that stops matching the selector is no longer kept by this window, as if a `stopTriggers` expression had matched.
- `conditionType` (string): the kind of maintenance this window does, as a node condition type
  (for example `kumaco.kured.dev/RebootInProgress`). The condition is `True` on a node exactly
  while that kind of maintenance is in progress on it, and absent otherwise. It is kumaco's
  only record of that fact (see "The node is the record").
  A consumer doing disruptive work (kured, a firmware updater, ...) must act on
  the condition of its own kind, not on `MaintenanceInProgress`: the latter only says that
  some maintenance is in progress on the node, not which kind, nor that this consumer's
  triggers and concurrency were checked.
  Several windows can have the same `conditionType` (weekdays and weekend windows for
  reboots, for example). They are then windows _of the same kind_: they work on the same
  records, and the consumer watches a single condition type, whatever the number of windows.
  Use one type per kind of maintenance, not one per window.
  The type must start with the controller's reserved prefix
  (`kumaco.kured.dev/` by default, configurable per controller), which is enforced by the
  controller (otherwise the window is `Invalid`), and by CRD validation for the default prefix. Without this, a
  maintenance manager, who has no node write access, could make the controller write or delete
  conditions owned by others (`Ready`, `detectors.kured.dev/RebootRequired`, ...).
  The reserved prefix also lets the controller recognise its own conditions (see startup).
- `startTriggers` (list): A list of CEL expressions that need to ALL apply to **start** maintenance on a node.
  They are only evaluated for nodes that are not yet under this kind of maintenance.
  Triggers (start and stop) can read the node's name, labels, annotations, spec (including whether it is cordoned via `node.spec.unschedulable`), and `node.status.conditions`. Other node status fields are not available to CEL.
  Expressions are compiled when the window is validated (a compile error makes the window
  `Invalid`) and run with a cost limit and an evaluation timeout. An expression that errors or
  times out for a node emits an event and fails closed, meaning it never changes whether the node is under maintenance:
  - in `startTriggers`, maintenance does not start on the node (`UnmetTrigger`);
  - in `stopTriggers`, for a node not yet under maintenance, maintenance does not start on the node (`UnmetTrigger`);
  - in `stopTriggers`, for a node already under maintenance, that expression does not release the node.
- `stopTriggers` (optional, list): CEL expressions, of which ANY applying makes this window stop
  keeping a node under maintenance. The node is then **released**, which frees its concurrency
  slot, unless another window of the same kind still keeps it. Start conditions are
  often not release conditions: a node should not lose its maintenance because a blocker
  appeared after the drain started, but it must leave as soon as its reason disappears
  (for kured, `RebootRequired` is no longer `True`).
  Without `stopTriggers`, no node condition ever releases a node: it stays under maintenance until
  the window stops being `InProgress` (its `duration` elapsed).
  A node whose `stopTriggers` match does not start maintenance either, so it cannot be released and
  started again in a loop.
- `concurrency` (int): the maximum number of nodes, among the nodes this window selects, under
  this window's kind of maintenance at the same time. Nodes started by another window of the
  same kind count too: two reboot windows with `concurrency: 1` on the same nodes reboot one
  node at a time, not two.
  Windows of different kinds do not count each other's nodes, and there is no cluster-wide
  cap across kinds. A node can be under several kinds of maintenance at the same time
  (depends on the triggers). See concurrency conflicts below.
  Lowering `concurrency` never releases nodes already under maintenance: it only
  prevents new starts until the count falls below the new value.

Daylight saving time follows the configured timezone without special handling: if a
scheduled local time does not exist when clocks move forward, that occurrence does not
start; if a local time repeats when clocks move back, both occurrences start. Even a
fixed-year schedule can therefore start twice on that date. Overlapping occurrences
keep the window `InProgress` until the last one ends.

The MaintenanceWindow CRD's status is a report: the controller writes it and never reads it
back (what is under maintenance is recorded on the nodes). Its size does not depend on the number
of nodes. It contains:
- `state` (enum): This is a summarized result, determined based on spec and conditions.
  State is a practical way for users to check the overall state,
  rather than checking all the conditions independently.
  This is particularly useful when the maintenance manager sets complex CEL rules.
  `state` can be:
  - `InProgress`: the current time falls within a scheduled occurrence and its duration.
  - `Scheduled`: no occurrence is active, but a future occurrence exists.
  - `Closed`: no occurrence is active and no future occurrence exists (including a one-off
    window whose duration has elapsed).
  - `Invalid`: the input of the CRD is incorrect (unparsable schedule, CEL compile error,
    `conditionType` without the reserved prefix).
    An `Invalid` window starts no maintenance and keeps no node under maintenance.
- `nodesUnderMaintenance` (list): the names of the nodes this window selects that are under its
  kind of maintenance. It stays small: it is bounded by the highest `concurrency` of the windows of that kind.
- `matchingNodes` (int): the number of managed nodes matching `nodeSelector`.
- `nodeCounts` (map): while the window is `InProgress`, how many of the matching nodes are in each situation:
  - `UnderMaintenance`: under this window's kind of maintenance;
  - `UnmetTrigger`: the triggers do not allow maintenance to start;
  - `MaxConcurrencyReached`: the triggers allow it, but no slot is free.

The situation of one particular node is not in the window's status but on the node itself: see "Debugging".

### What does it reconcile, and when?

The controller watches:

1. cluster-wide custom resources `MaintenanceWindow` (matching a certain label if configured for it, for example in the case of multiple kumaco deployments);
2. node changes;
3. time-based requeues.

Queue reconciliation happens on scheduled boundaries, periodically, when a MaintenanceWindow
is created, changed, or deleted, or even if a relevant node input changes.

Reconciliations read nodes from the shared informer cache, never with an API list.
A node event only enqueues the windows whose `nodeSelector` matches that node before or
after the change (a window not selecting it cannot be affected).

Node changes may affect selectors or CEL triggers.
Therefore:

1. All spec changes are therefore relevant.
2. From the metadata perspective, only labels and annotations are relevant.
3. From the status perspective, only Conditions are considered relevant (excluding kumaco's own `MaintenanceInProgress` changes).

If a fact from another node status field should affect maintenance, its reporter must
publish it as a node condition, for example with node-problem-detector. CEL triggers
then read that condition rather than the original status field.

kumaco's own condition writes do not requeue anything through node events. Instead, when a
reconcile starts or releases a node, the controller enqueues the other
windows selecting that node, so that a trigger reading `MaintenanceInProgress` (or the
condition of another kind) is reevaluated. The controller reads back its own writes before
reconciling again (see "Reading its own writes"), so triggers see the latest decisions.

### What does it look like on my node?

Most of the time:

```
type: `MaintenanceInProgress`
status: False
reason: KumacoNoMaintenance
message: "No maintenance in progress."
lastTransitionTime: <time the condition became False>
lastHeartbeatTime: <recent time showing controller liveness>
```

While a window selecting the node is `InProgress`, but has not started maintenance on it,
the message says why:

```
type: `MaintenanceInProgress`
status: False
reason: KumacoNoMaintenance
message: "No maintenance in progress. Not started: reboots-weekdays (MaxConcurrencyReached)"
```

During a maintenance, the node has the generic condition and the condition of the kind of
maintenance (the window's `conditionType`):

```
type: `MaintenanceInProgress`
status: True
reason: KumacoMaintenanceRunning
message: "In progress: kumaco.kured.dev/RebootInProgress. Not started: firmware-q4 (UnmetTrigger)"
lastTransitionTime: <time the condition became True>
lastHeartbeatTime: <recent time showing controller liveness>
```

```
type: `kumaco.kured.dev/RebootInProgress`
status: True
reason: KumacoMaintenanceRunning
message: "Started by MaintenanceWindow reboots-weekdays"
lastTransitionTime: <time this kind of maintenance started on the node>
lastHeartbeatTime: <recent time showing controller liveness>
```

`MaintenanceInProgress` is a generic signal: some maintenance is in progress on this node.
Consumers doing disruptive work act on the condition of their own kind (kured, for example,
only reboots on `kumaco.kured.dev/RebootInProgress`). That condition is present only while
that kind of maintenance is in progress.

Messages are for humans: nothing parses them, kumaco included. The "Not started" part is
refreshed with the next write to the node, at the latest the next heartbeat, so it can be
one heartbeat interval late.

### Heartbeat contract

`lastHeartbeatTime` on kumaco's conditions lets consumers tell a live controller from a dead
one, because a dead controller leaves `True` in place forever.
- The controller refreshes `lastHeartbeatTime` on every condition it owns when it is older
  than `--heartbeat-interval` (default 5m, with jitter), in the same patch as any other change.
- Consumers should treat a `True` condition whose heartbeat is older than 3 intervals
  (15m by default) as **not authorizing new work**.
- In-flight work is the consumer's decision. kured, for example, should finish a reboot it
  already started.

### Condition types and their owners

kumaco writes some node condition types, and its documentation relies on others. Each type
has exactly one owner, which defines its name and meaning:

| Condition type | Owner | Use by kumaco |
|---|---|---|
| `MaintenanceInProgress` | Kubernetes ([node lifecycle conditions](https://kubernetes.io/blog/2026/09/09/kubernetes-v1-37-node-lifecycle-conditions/)) | Written by kumaco. Kubernetes leaves setting and clearing it to the administrator, a role kumaco takes on. |
| `kumaco.kured.dev/*`, for example `kumaco.kured.dev/RebootInProgress` | [kubereboot/kumaco](https://github.com/kubereboot/kumaco) (the reserved prefix) | Written by kumaco. `kumaco.kured.dev/RebootInProgress` is consumed by [kured](https://github.com/kubereboot/kured). |
| `Ready` | Kubernetes (the kubelet) | Read by the documented triggers. |
| `detectors.kured.dev/RebootRequired` | [kubereboot/node-reboot-required-reporter](https://github.com/kubereboot/node-reboot-required-reporter) | Read by the documented triggers. |
| `detectors.kured.dev/NoPodInhibitor`, `detectors.kured.dev/NoAlertInhibitor` | [kubereboot/reboot-inhibitors-reporter](https://github.com/kubereboot/reboot-inhibitors-reporter) | Read by the documented triggers. |
| `example.com/CNIReady`, `firmware.example.com/UpdateRequired` | Nobody: examples for the node readiness controller and node problem detector | None. |

Changing the name or meaning of a type starts with an issue in its owner's repository. The
owner's release, this documentation, and kumaco's e2e pins then change together, so that kumaco
never documents or tests a condition its owner no longer publishes.

### How does it work?

#### The node is the record

A controller only manages the nodes matching its node scope (`--node-selector`, a label
selector; the default, all nodes, is only safe with a single controller). Window selectors
are evaluated within this scope. "Managed nodes" below always means this set. A node leaving
the scope keeps its last conditions: clear them before relabeling it.

kumaco stores nothing of its own. A window declares its kind of maintenance with
`conditionType`. "Node N is under this kind of maintenance" is recorded in exactly one
place: that condition is `True` on N. The condition's `lastTransitionTime` is the time
this maintenance started on N.

The record belongs to the kind, not to one window: windows of the same kind work on the
same records, whichever of them started the maintenance.

Everything else is derived from these records, read through the informer cache:
- the nodes under a kind of maintenance: the managed nodes carrying its condition;
- a window's used concurrency: the number of those nodes among the nodes it selects;
- `MaintenanceInProgress` on a node: `True` if at least one such condition is on it, `False` otherwise;
- the window's `status`: a report of the above.

This is what lets the controller resume after a problem:
- After a crash, a restart, or a leader change, there is nothing to rebuild or replay. The
  records are in the API server, and the new process reads them like any other node data.
- A deleted node takes its records with it. A node recreated with the same name has none.
- A node write that fails leaves no record, so the next reconcile decides again from the
  start, triggers included.

The controller may keep derived data in memory (indexes, metric values, the last situation
of each node for the messages), as long as it can be dropped and recomputed at any time.
No decision depends on it.

#### One function, one write per node

A single function computes the complete list of kumaco conditions a node must have.
Its inputs are the node as seen in the cache, the existing windows, and the decisions of the
reconcile in progress (none, for the sweep). It uses no other state:

```
desiredConditions(node, started, released):
    kinds = kumaco condition types (reserved prefix) that are True on the node   # the records
    kinds = kinds - types that no existing window has as conditionType           # leftovers
    kinds = kinds - released + started                                           # decisions of this reconcile

    list = one condition per kind: status True, reason KumacoMaintenanceRunning
    list += MaintenanceInProgress:
              True  / KumacoMaintenanceRunning  if kinds is not empty
              False / KumacoNoMaintenance    otherwise
    return list
```

For example, with a reboot window and a firmware window:

| Event | Result of `desiredConditions(node)` | Effect of the write |
|---|---|---|
| nothing in progress | `MaintenanceInProgress: False` | none (already there) |
| the reboot window starts on the node | `RebootInProgress: True`, `MaintenanceInProgress: True` | one condition added, one flipped |
| the firmware window starts on the node too | `RebootInProgress: True` (carried over), `FirmwareInProgress: True`, `MaintenanceInProgress: True` | one condition added |
| the reboot `stopTriggers` match | `FirmwareInProgress: True`, `MaintenanceInProgress: True` | `RebootInProgress` removed |
| the firmware window is deleted while kumaco is down | `MaintenanceInProgress: False` (no window has the type anymore) | `FirmwareInProgress` removed by the startup sweep |

Every node write sends this complete list in one server-side apply on `nodes/status`, with
the field manager `kumaco`. Never send a subset: server-side apply removes what a field
manager stops sending. This is also the only cleanup mechanism: what is not in the list
anymore (released node, deleted windows, changed `conditionType`) is removed by the same write.

Only kumaco-owned condition types are sent, so other reporters' conditions are preserved and
ownership is explicit. Never replace the whole conditions list.
(Check that `NodeStatus.conditions` is a map-type list, keyed by `type`, in all supported Kubernetes versions;
otherwise fall back to a strategic merge patch.)

Rules for these writes:
- `lastTransitionTime` is kept unless the `status` changes. A new condition gets the current time.
- A write that starts maintenance on a node (a condition appears) sets
  `metadata.resourceVersion` to the version the decision was based on, so a concurrent change
  to the node (for example the reboot reporter) rejects the write. Nothing is recorded then,
  and the window is requeued: its `startTriggers` are evaluated again on the fresh node.
- Releases and heartbeats are applied without that precondition, as they are always safe.
- A reconcile writes nothing when the node already has the desired types and statuses.
  A difference in `message` alone does not cause a write: it goes with the next one.
- Heartbeat timestamps are refreshed when due, in the same apply (see "Heartbeat contract").
  Heartbeat-only node changes do not requeue windows (they are filtered).

#### Reading its own writes

The informer cache can lag behind the controller's own writes. Counting nodes
from a cache that does not show the last start yet would exceed `concurrency`. So the
controller does not reconcile a window until its cache shows all the node writes it made
(the "expectations" pattern of the ReplicaSet controller).

#### Startup and periodic sweep

At container start, or on gaining leadership:
1. Wait for the informer caches (nodes and windows) to sync.
2. Sweep: for every managed node, compute `desiredConditions(node)` and apply it if it
   differs. This removes the records whose type no window has anymore (windows deleted or
   changed while kumaco was down), and sets `MaintenanceInProgress` where it is missing or outdated.
   This first sweep does not refresh heartbeats.
3. Reconcile every window once.
4. Only then start refreshing heartbeats.

The sweep does not evaluate schedules or triggers. Whether a record is still justified is
decided by the windows' reconciles. This is why heartbeats wait for step 3: a record left
over from before the downtime must not look fresh to consumers before it has been checked.

The same sweep then runs at every heartbeat interval. It refreshes heartbeats and messages,
and gives `MaintenanceInProgress: False` to new nodes that no window selects.

#### When does a window keep a node under maintenance?

A window _keeps_ a node that is under its kind of maintenance when all of these are true:
- the window is `InProgress`;
- the node matches its `nodeSelector`;
- the record is not older than the start of the window's current occurrence;
- NONE of its `stopTriggers` matches (an expression that errors does not match).

A node is **released** (its record is removed) when no window of that kind keeps it.

The third rule is about records that outlived their occurrence. Example: a daily window from
02:00 to 04:00. On Monday at 02:10, maintenance starts on node A. The controller dies at 03:00
and only comes back on Tuesday at 02:05. Nobody removed A's record on Monday at 04:00, so A
still carries it, and Tuesday's occurrence is now in progress. Without this rule, A would just
stay under maintenance, although Tuesday's `startTriggers` were never evaluated for it. With
this rule, the record (Monday 02:10) is older than the current occurrence (Tuesday 02:00):
A is released, and is a normal candidate again.

No special requeue is needed for this. Requeues at schedule boundaries are in-memory timers,
so Monday's 04:00 one died with the controller. On Tuesday, the startup reconciles every
window once (step 3 of the startup), and the rule is checked there, as in every reconcile.
A stale heartbeat is deliberately not used for this: it would also release nodes after a short
outage inside the same occurrence, in the middle of their maintenance.

If occurrences of a window overlap (its `duration` is longer than the time between two
starts), its "current occurrence" starts at the earliest occurrence of the overlapping chain.

A consequence for windows of the same kind: when a window ends, the nodes it started are only
kept by another window if they started during that other window's current occurrence.
Otherwise they are released, and can start again right away if its `startTriggers` match.

#### Reconciling a window

Windows are reconciled one at a time. The current time is read once, at the start of the reconcile.

Case 1: Deletion of a maintenance window.
Run the second step (release) for its kind, then the fifth step. If it was the last window
of its kind, `desiredConditions` drops the records of that kind.

Case 2: Every other event.

First step - Validate and evaluate the schedule:
- The window is `Invalid` if its spec cannot be used (see `status.state`).
- The window is `InProgress` if now is between an occurrence's start and its start plus `duration` (regardless of whether there is a future start)
- The window is `Scheduled` if no occurrence is active and a next start > now exists
- The window is `Closed` if no occurrence is active and no future start exists

If the window is not `InProgress`, skip the third and fourth steps.

Second step - Release:
This step runs before any start, so that the slots it frees can be reused in the same reconcile.
It goes over all the managed nodes carrying the window's `conditionType`, whichever window started them.
A node is released unless at least one window of that kind keeps it (see above).
`startTriggers` are never evaluated for a node already under this kind of maintenance.

Third step - Find the candidates:
This step goes over the managed nodes matching `mw.nodeSelector` that are not under this kind of maintenance.
A node is a candidate only if ALL `mw.startTriggers` match and NONE of `mw.stopTriggers` matches
(an expression that errors makes the node not a candidate).
The other nodes are counted as `UnmetTrigger`.

Fourth step - Fill the free slots:
The number of free slots is `mw.spec.concurrency` minus the number of nodes matching `mw.nodeSelector`
that are still under this kind of maintenance after the second step.
If it is zero or less (`concurrency` was lowered), nothing starts and nothing is released.

Candidates are ordered as follows:
1. First the candidates already under another kind of maintenance (`MaintenanceInProgress` is `True`),
   most recent `lastTransitionTime` first.
   The user has decided to _not exclude them_ (this can be done with a `startTriggers` expression requiring
   `MaintenanceInProgress` to not be `True`), so we consider the user wants to _combine_ maintenances
   on the same node rather than take one more node out of service.
2. Then the other candidates ("standalone"), oldest `MaintenanceInProgress` `lastTransitionTime` first:
   the node that left maintenance the longest time ago goes first. Nodes without the condition
   yet are ordered by creation time, older nodes first.

Ties are broken by node name so that the order is deterministic.

Maintenance starts on the first candidates, up to the number of free slots. The remaining
candidates are counted as `MaxConcurrencyReached`.

Fifth step - Write the nodes:
For every node released or started, apply `desiredConditions(node)` (see "One function, one write per node").
A start whose write is rejected is not a start: nothing is recorded, and the window is requeued.

Sixth step - Report:
- Update the MaintenanceWindow status from what was actually written.
- Remember each matching node's situation for its `MaintenanceInProgress` message.
- Emit events (see "Debugging") and update the metrics.
- Enqueue the other windows selecting the nodes released or started.
- Requeue this window at its next schedule boundary.

A status update that fails is retried. It changes no decision, as the status is never read back.

### Handling MaintenanceWindow conflicts

#### Scheduling conflicts

If two maintenance windows overlap, only one is sufficient to be considered as _under maintenance_.
In other words if you have two MaintenanceWindows A and B applied on all the nodes of a cluster:

A might be in state Closed/Scheduled
B might be in state InProgress

--> B alone decides: the nodes B starts (triggers, concurrency) are _under maintenance_. A's state does not prevent it.

#### Triggering conflicts

For example, A is active but its trigger does not match; B's trigger matches but
B is closed. Neither window makes the node eligible.

If B becomes active and selects the node, B alone is sufficient to set the
node under maintenance.

#### nodeSelector and trigger conflicts

The nodeSelector has _precedence_ over trigger.

Let's take the following scenarios:

First case, a MaintenanceWindow has a nodeSelector for all nodes, with a trigger matching node.metadata.name == 'node1'.
In this case, the triggered case will be true on node1, false on all other nodes.
Only node1 will have the resulting `MaintenanceInProgress` condition.

Second case, a MaintenanceWindow has a selector to only match node1, with a trigger matching all nodes.
In this case, the triggered case will be true on node1, false on other nodes
Only node1 can have the resulting `MaintenanceInProgress` condition, because the same window must select it and match its triggers while active.

Third case, a MaintenanceWindow has a selector to match node1, with a trigger matching only node2.
No node has the resulting `MaintenanceInProgress` set to True.

#### Concurrency conflicts

Windows are reconciled one at a time, and the controller reads back its own node writes
before the next reconcile. A window therefore always counts nodes from up-to-date
records, and no concurrency conflicts should happen:
- Each window counts, among the nodes it selects, the nodes carrying its `conditionType`.
- Windows of the same kind count the same records, so they never add up their `concurrency` on the same nodes.
- A node under several kinds of maintenance uses one slot in each kind.

### Multi-tenancy while doing self-service is *not an afterthought*

This solves the following use case:
"As a maintenance manager of team X, I want to reboot all my team X nodes but not team Y nodes".

Each team owns a separate, non-overlapping group of nodes and runs one kumaco controller
for that group (its node scope, `--node-selector`). The cluster administrator must keep the groups disjoint: two controllers
writing the same node conditions would overwrite each other's decisions. Each controller
has its own concurrency pool.

#### Replicas of the same controller

Replicas of the same named controller (managing the same windows and node group)
share a leader-election Lease:
only the leader reconciles and writes MaintenanceWindow status and node conditions.
The leader processes window reconciliations sequentially. A new leader waits for its
API view to sync, then runs the startup sweep before ordinary reconciliation. It needs
nothing from the previous leader: what is under maintenance is recorded on the nodes. A replica stops writing when it
loses leadership. Node write conflicts require refetching and recomputing, not replaying
a stale decision. Leader election alone does not fence an in-flight write during a
handover; the controller converges through conflict checks and reconciliation, not an
atomic update of all nodes. Controllers for disjoint teams use separate Leases and
can run at the same time.

#### The harder approach: the service-account limitations

If you have a kubernetes cluster used by different teams, you can do this by running
n deployments of kumaco, each of them with their own serviceAccount.

For fixed node groups, grant each controller ServiceAccount `nodes/status` write rights
only for its team's node names (using RBAC `resourceNames`). Do not grant the same node
to two controllers. When moving a node between teams, clear the old controller's
conditions before revoking its write access, then grant access to the new controller.
The controller can watch node changes with read-only access, but must not write status on
nodes outside its group.

#### The softer approach: the controller watch label

If you have a kubernetes cluster used by different teams, you can do this by running
n deployments of kumaco, each of them with their watcher label.

Kumaco will only watch for MaintenanceWindows matching the label configured for its team.
This label alone is not a node access boundary; the administrator must ensure the teams'
node groups do not overlap.

In other words, the administrator has configured the kumaco controller deployment for team A
to only watching for MaintenanceWindow CRDs matching the label "MaintenanceWindowTeam: A".
The administrator has configured another kumaco controller deployment, for team B, only
watching for the MaintenanceWindow CRD matching the label "MaintenanceWindowTeam: B".

For this approach, the administrator must enforce an admission policy (with tools like
kubewarden, kyverno, or OPA) tying the maintenance manager's identity to:
- their team;
- the MaintenanceWindow's team label;
- a nodeSelector that includes an administrator-controlled team label matching only
  that team's nodes.

Kubernetes RBAC cannot restrict `nodes/status` writes by node label. If write access is
not restricted to named nodes, the administrator must trust the deployment and its
admission policy to preserve the separation between teams.

### How kured relies on kumaco, and how to configure your maintenance window for kured:

Basic configuration for kured is the following:

1) Set the maintenance window time
2) Set the nodeSelector
3) Set the following CEL `startTriggers` (all must match to start maintenance on a node):

```cel
node.status.conditions.exists(c, c.type == 'detectors.kured.dev/RebootRequired' && c.status == 'True')
node.status.conditions.exists(c, c.type == 'detectors.kured.dev/NoPodInhibitor' && c.status == 'True')
node.status.conditions.exists(c, c.type == 'detectors.kured.dev/NoAlertInhibitor' && c.status == 'True')
```

This example assumes you are running the blocker reporter, which publishes explicit `NoPodInhibitor` and
`NoAlertInhibitor` conditions.
This component set the value to `True` only when it has checked and found no blocker.
For these triggers, missing, `False`, and `Unknown` do not authorize maintenance.

Also set this `stopTriggers`, so that kured nodes are released once the reboot is done and the node is back:

```cel
!node.status.conditions.exists(c, c.type == 'detectors.kured.dev/RebootRequired' && c.status == 'True') &&
node.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True')
```

An inhibitor appearing after kured started draining is too late to matter: only `startTriggers` look
at inhibitors, so it does not release the node. The node and its concurrency slot are released when
`RebootRequired` is no longer `True` (set to `False`, or removed) and the node is `Ready` again.
While the node reboots, the stale `RebootRequired`, then `Ready` not being `True`, keep it under
maintenance. Without this `stopTriggers`, the node would keep its slot until the window ends.

Cordoning is deliberately not part of this trigger. `Ready` is reported by the kubelet and does not
change when a node is cordoned (only `node.spec.unschedulable` does). The same trigger therefore
covers both cases:
- a schedulable node, which kured cordons, drains, reboots and uncordons;
- a node that was already cordoned before the maintenance, which kured reboots but leaves cordoned.

4) Set `conditionType` to `kumaco.kured.dev/RebootInProgress` on **every** reboot window, and configure kured
to act on that condition. However many reboot windows you define (weekdays, weekend, per node pool, ...),
kured watches this single condition type, and their `concurrency` is not added up on the nodes they share.
kured must not reboot on `MaintenanceInProgress` alone: another kind of maintenance (firmware, ...)
can be in progress on the node without having checked kured's inhibitors or concurrency. The heartbeat contract
applies to this condition too. kured only reads it: it needs no write access to `nodes/status`.

Other triggers may explicitly match a missing or `Unknown` condition, for example:

```cel
!node.status.conditions.exists(c, c.type == 'detectors.kured.dev/NoPodInhibitor')
node.status.conditions.exists(c, c.type == 'detectors.kured.dev/NoPodInhibitor' && c.status == 'Unknown')
```

Each expression above matches only its stated case; it does not override the schedule,
selector, or other triggers. If a reporter publishes a condition only when blocked and
removes it otherwise, absence cannot distinguish "safe" from "not checked yet". The
positive `NoPodInhibitor` trigger above requires the reporter to report readiness.

#### Using kumaco with the node readiness controller

The [node readiness controller](https://github.com/kubernetes-sigs/node-readiness-controller)
(a `NodeReadinessRule` lists the node conditions that must have a given status and keeps a taint,
for example `NoSchedule`, on the selected nodes until they do) and kumaco do not talk to each other.
They are complementary, because both only use node conditions:
- kumaco decides **when a node may leave service** (maintenance window, concurrency).
- the node readiness controller decides **when a node may take workloads again**, by keeping its
  taint until the required conditions (CNI, storage, GPU driver, ... reported by node-problem-detector
  or a custom daemon) are `True`.

Without coordination, kumaco frees the slot as soon as the reboot reason is gone, and may start
the next node while the previous one is not yet usable. To make kumaco wait, reuse the conditions
the readiness rule requires in `stopTriggers`. A node then keeps its slot until it is really back:

```cel
!node.status.conditions.exists(c, c.type == 'detectors.kured.dev/RebootRequired' && c.status == 'True') &&
node.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True') &&
node.status.conditions.exists(c, c.type == 'example.com/CNIReady' && c.status == 'True')
```

If the node never becomes ready, it holds its slot (and so stops the rollout) until the window ends:
this is intended, `concurrency` bounds the damage and the window `duration` bounds the wait.
Also add `Ready` and the same required conditions to `startTriggers` if you do not want to
start maintenance on a node that is already unhealthy.

Use the readiness rule's continuous enforcement mode, not `bootstrap-only`: a rebooted node is no longer
bootstrapping, and `bootstrap-only` stops re-evaluating the node once it has been ready.
This is a usage pattern, not an integration documented by the node readiness controller;
check the rule's fields against its current API version (it is `v1alpha1`).

This integration is not in the e2e test yet, please feel free to contribute.

#### Using kumaco with node problem detector

[Node problem detector](https://github.com/kubernetes/node-problem-detector) (NPD) is the usual
reporter for facts that are not in `node.status` fields CEL can read: kumaco triggers only see
node conditions, labels, annotations and spec (see the CRD section). NPD publishes permanent
problems as node conditions (temporary ones are only events, which kumaco cannot read), and its
custom plugin monitor runs your own check script and maps its result to a condition. So any
"this node needs maintenance" signal (pending security patches, outdated firmware, a degraded
disk, ...) becomes a trigger once a custom plugin reports it as a condition.

For example, if a plugin publishes the condition `firmware.example.com/UpdateRequired`:

```cel
node.status.conditions.exists(c, c.type == 'firmware.example.com/UpdateRequired' && c.status == 'True')
```

Use it in `startTriggers` to start maintenance on the node, and its negation in `stopTriggers` to release the node
once the check passes again.

Things to keep in mind:
- Report readiness explicitly, as with the kured blocker reporter. NPD's default conditions are
  `False` when healthy and `True` when the problem is present; a missing condition means "not checked yet",
  not "healthy". Pick the polarity your trigger needs, and do not rely on absence.
- NPD writes conditions on `nodes/status` too. Condition types owned by NPD are not under kumaco's
  reserved prefix; kumaco only writes `MaintenanceInProgress` and its own prefixed window conditions.
- NPD stops updating conditions if it dies or cannot reach the API server. A stale `True` keeps
  matching, and triggers cannot detect it (CEL has no clock here, and heartbeat-only changes do not
  requeue), so monitor NPD's liveness separately if staleness matters.
- Kumaco's `MaintenanceInProgress` is itself consumed by NPD to tell an administrated node from
  an unexpected failure (see KEP-5683, "Node Problem Detector" story); NPD health conditions can still be
  `True` on a node under maintenance.

This integration is not in the e2e test yet, please feel free to contribute.

#### Explanation of interconnection with kured

This controller can not only be used to _define a condition on nodes_,
it can be used as _the brain of maintenances_.

In the kured case, we wanted to have a source of truth to know if that node
was in maintenance to _trigger the rest of the process_
(Drain/Cordon/Reboot/Uncordon), which is done by another controller.
That source of truth is the kured windows' condition (`kumaco.kured.dev/RebootInProgress`
in the example above), not the generic `MaintenanceInProgress`.

Kured requires kumaco to:
- Check whether the node is in a maintenance windows (core feature of kumaco)
- Check whether the node has other positively matching conditions (core feature of kumaco):
  the node needs `detectors.kured.dev/RebootRequired` set to `True` in order to start the Maintenance
  (and set `MaintenanceInProgress` and the window's condition).
- Check that neither a pod nor an alert inhibits the reboot, using the positive conditions shown above.
  With these triggers, an absent or `Unknown` inhibitor report does not authorize maintenance.
  This is also a core feature of kumaco.
- Restrict how many nodes can be under maintenance at the same time for a maintenance.
  This is also a core feature of kumaco, restricting the amount of nodes per maintenance
  window, and bundling the maintenance window together if possible.

Kured then only focuses on setting new conditions/labels/annotations on nodes and rebooting.
Those node changes requeue kumaco reconciliations, which is fine: a reconcile that changes nothing writes nothing.

## Metrics

Metrics report the same decisions as reconciliation. They are derived from the records on the nodes, so they are correct again as soon as the caches are synced after a restart.

Each controller exports:

- `kumaco_maintenance_window_active{window}`: gauge, 1 while that window's schedule is
  active, 0 otherwise.
- `kumaco_maintenance_window_nodes{window,state}`: gauge, number of matching nodes per situation
  (same values as `status.nodeCounts`).
- `kumaco_node_slot_occupied{node}`: gauge for each managed node, 1 if
  `MaintenanceInProgress` is `True`, 0 otherwise.
- `kumaco_maintenance_window_invalid{window}`: gauge, 1 while the window is `Invalid`.
- `kumaco_reconcile_total{window,result}`: counter of reconciles (`result` is `ok` or `error`).
- `kumaco_reconcile_duration_seconds`: histogram of reconcile duration.
- `kumaco_node_write_total{result}`: counter of node condition writes (`ok`, `conflict`, `error`).
- `kumaco_oldest_heartbeat_age_seconds`: gauge, age of the oldest owned condition heartbeat
  (alert when it exceeds the heartbeat interval by a wide margin).
- `kumaco_leader`: gauge, 1 on the replica currently leading, 0 otherwise.

## Gotchas

A `stopTriggers` expression depending on a condition this window sets can oscillate.
For example, releasing when `MaintenanceInProgress` is `True` releases the node right after its maintenance started, then starts it again.
`startTriggers` cannot oscillate this way, because they are not evaluated for nodes already under this kind of maintenance:
requiring `MaintenanceInProgress` to not be `True` is the way to refuse nodes under another kind of maintenance (see the fourth step).

The user should not write self-referential `stopTriggers`. We do not validate it. An administrator can use an admission rule to prohibit them; reading the condition of a different kind of maintenance can be intentional.

When a window is deleted, the nodes it was keeping are released, unless another window of
the same kind keeps them. If the deletion (or a `conditionType` change) happened while kumaco
was down, the startup sweep and the first reconciles do the same. Consumers should
apply the heartbeat contract to every kumaco condition, not only to `MaintenanceInProgress`.

Do not create one `conditionType` per window: consumers would have to follow every new type.
A type names a kind of maintenance (reboot, firmware, ...), shared by all its windows.

## Debugging with kumaco events

"Why is my node not under maintenance?" is answered on the node, for the _maintenance manager_
(who needs read access to nodes for this): the message of its `MaintenanceInProgress` condition
lists the `InProgress` windows selecting the node that did not start, and why
(`UnmetTrigger`, `MaxConcurrencyReached`). See "What does it look like on my node?".
It costs no extra write: the message travels with the heartbeat.

The window's `status.nodeCounts` (and the matching metric) tell how many nodes are in each situation.

kumaco emits events on the MaintenanceWindow. They are only emitted when something changes,
so their number follows `concurrency`, not the number of nodes:
- maintenance started on a node;
- a node was released, and why (stop trigger, selector, end of the window, invalid or deleted window);
- a trigger expression errored or timed out for a node;
- a start was rejected because the node changed concurrently.

kumaco also displays actions in its logs, which are intended for the _kumaco deployer_, to debug the controller.

## Security and access rights

Maintenance managers need permission to manage their team's MaintenanceWindows, not
permission to write nodes. Read access to nodes is optional, but lets them see why a node is
not under maintenance (see "Debugging"). Each controller ServiceAccount needs to watch its windows,
read nodes, and update `nodes/status` for its team's nodes. RBAC can restrict writes to
named nodes with `resourceNames`, but not to particular node condition types or to
nodes matching a label. Grant only the node names that controller should manage; a
compromised controller can change *any* status condition on those allowed nodes.
Because maintenance managers choose `conditionType`, it must be restricted to the
controller's reserved prefix (`kumaco.kured.dev/` by default).

## Getting Help

If you have any questions about, feedback for or problems with `kumaco`:

- Invite yourself to the <a href="https://slack.cncf.io/" target="_blank">CNCF Slack</a>.
- Ask a question on the [#kured](https://cloud-native.slack.com/archives/kured) slack channel (kumaco does not have its own channel yet)
- [File an issue](https://github.com/kubereboot/kumaco/issues/new).
- Join us in [kured monthly meeting](https://docs.google.com/document/d/1AWT8YDdqZY-Se6Y1oAlwtujWLVpNVK2M_F_Vfqw06aI/edit),
  every first Wednesday of the month at 16:00 UTC.
- You might want to [join the kured-dev mailing list](https://lists.cncf.io/g/cncf-kured-dev) as well.

We follow the [CNCF Code of Conduct](CODE_OF_CONDUCT.md).

Your feedback is always welcome!

## Trademarks

**Kured is a [Cloud Native Computing Foundation](https://cncf.io/) Sandbox project.**

![Cloud Native Computing Foundation logo](img/cncf-color.png)

The Linux Foundation® (TLF) has registered trademarks and uses trademarks. For a list of TLF trademarks, see [Trademark Usage](https://www.linuxfoundation.org/legal/trademark-usage).
