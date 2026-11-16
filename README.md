# kumaco - Kubernetes Maintenance Controller

- [Introduction](#introduction)
- [Behaviour](#behaviour)
  - [The personas](#the-personas)
  - [The kuma.kured.dev MaintenanceWindow CRD](#the-kumakureddev-maintenancewindow-crd)
  - [What does it reconcile, and when?](#what-does-it-reconcile-and-when)
  - [What does it look like on my node?](#what-does-it-look-like-on-my-node)
  - [Heartbeat contract](#heartbeat-contract)
  - [How does it work?](#how-does-it-work)
  - [Handling MaintenanceWindow conflicts](#handling-maintenancewindow-conflicts)
  - [Multi-tenancy while doing self-service](#multi-tenancy-while-doing-self-service-is-not-an-afterthought)
  - [How kured relies on kumaco](#how-kured-relies-on-kumaco-and-how-to-configure-your-maintenance-window-for-kured)
- [Metrics](#metrics)
- [Gotchas](#gotchas)
- [Debugging](#debugging-with-kumaco-events)
- [Security and access rights](#security-and-access-rights)
- [Getting Help](#getting-help)
- [Trademarks](#trademarks)

This code is in early alpha.

## Introduction

kumaco (KUbernetes MAintenance COntroller) is a kubernetes controller
watching a "Maintenance" Custom Resource (declaring maintenance windows)
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
- [kured](https://github.com/kubereboot/kured) drains and restarts the nodes (it is running on all nodes, but with different privileges as node-reboot-required-reporter)

## Behaviour

### The personas

Two kind of people are involved in kumaco's deployment:
- kumaco's deployer, who has admin rights on the cluster. It deploys the kumaco controller (CRD, deployment, service account, service)
- kumaco's maintenance manager, who has administrative rights to put a node in/out of maintenance, but do not have access rights to nodes themselves.

### The kuma.kured.dev MaintenanceWindow CRD

A CRD allow us to:
- Validate input easily like any config map would
- Report the state of the MaintenanceWindow (`InProgress`, `Scheduled`, `Closed`, `Invalid`)
- Use RBAC to grant access to different people (a "maintenance manager" might have access to define the conditions/timing of a maintenance window, but might not have access to the node)
- Let people define their own admission events.

The MaintenanceWindow CRD's spec contains:
- `startTime`: a cron-like string.
  It is structured as the following:
  - an optional `CRON_TZ=<IANA timezone>`. Without `CRON_TZ`, the timezone is UTC.
  - seven Quartz-style fields: second, minute, hour, day of month, month, day of week, year.
  
  For example,
  `CRON_TZ=Europe/Berlin 0 0 2 1 1 ? 2027` starts once at 02:00 Berlin time on 2027-01-01;
  `CRON_TZ=Europe/Berlin 0 0 2 * * ? *` starts daily at 02:00 local time.

  The timezone prefix plus year-aware fields is kumaco's defined input format, not a
  universally standardized cron expression. No common cron library parses it, so kumaco
  needs its own parser. Because a window is active when `now` falls in
  `[start, start+duration)`, it must also find the latest occurrence at or before `now`
  (all of them, if occurrences overlap), not only the next one.

- `duration` (duration): maximum real elapsed time for a maintenance window. In other words: a maintenance window is active if between [startTime, startTime+duration]
- `nodeSelector` (kubernetes labelSelector): the nodes on which the maintenance window is applied to.
- `startTriggers` (list): A list of CEL expressions that need to ALL apply to **admit** a node into maintenance.
  They are only evaluated for nodes that are not yet under maintenance in this window.
  Triggers (start and stop) can read the node's name, labels, annotations, spec (including whether it is cordoned via `node.spec.unschedulable`), and `node.status.conditions`. Other node status fields are not available to CEL.
  Expressions are compiled when the window is validated (a compile error makes the window
  `Invalid`) and run with a cost limit and an evaluation timeout. An expression that errors or
  times out for a node counts as not matching (`UnmetTrigger`) and emits an event.
- `stopTriggers` (optional, list): CEL expressions, of which ANY applying **releases** a node already
  under maintenance in this window, which frees its concurrency slot. Admission conditions are
  often not release conditions: a node should not lose its maintenance because a blocker
  appeared after the drain started, but it must leave as soon as its reason disappears
  (for kured, `RebootRequired` is no longer `True`).
  Without `stopTriggers`, no node condition ever releases a node: it stays under maintenance until
  the window stops being `InProgress` (its `duration` elapsed), when all its nodes are released.
  A node whose `stopTriggers` match is not admitted either, so it cannot be released and
  re-admitted in a loop.
- `concurrency` (int): the maximum number of distinct nodes admitted for this maintenance window.
  A node already counted in another maintenance window might be reused for two maintenance at the same time (depends on their triggers). See concurrency conflicts below.
  There is no cluster-wide cap across windows: each window enforces its own `concurrency`.
  Lowering `concurrency` never evicts nodes already under maintenance in this window: it only
  prevents new admissions until the count falls below the new value;
- `extraConditionType` (optional, string): an extra node condition owned by this window,
  set when its node goes into or leaves maintenance. This can distinguish overlapping
  maintenance. Windows that can select the same node need different extra condition
  types for separate results. The type must start with the controller's reserved prefix
  (`kumaco.kured.dev/` by default, configurable per controller), which is enforced by CRD
  validation and again by the controller (otherwise the window is `Invalid`). Without this, a
  maintenance manager, who has no node access, could make the controller write or delete
  conditions owned by others (`Ready`, `detector.kured.dev/RebootRequired`, ...).
  The reserved prefix also lets the controller recognise its own conditions (see startup).

Daylight saving time follows the configured timezone without special handling: if a
scheduled local time does not exist when clocks move forward, that occurrence does not
start; if a local time repeats when clocks move back, both occurrences start. Even a
fixed-year schedule can therefore start twice on that date. Overlapping occurrences
keep the window `InProgress` until the last one ends.

The MaintenanceWindow CRD's status contains:
- `state` (enum): This is a summarized result, determined based on spec and conditions.
  State is a practical way for users to check the overall state,
  rather than checking all the conditions independently.
  This is particularly useful when the maintenance manager sets complex CEL rules.
  `state` can be:
  - `InProgress`: the current time falls within a scheduled occurrence and its duration.
  - `Scheduled`: no occurrence is active, but a future occurrence exists.
  - `Closed`: no occurrence is active and no future occurrence exists (including a one-off
    window whose duration has elapsed).
  - `Invalid`: the input of the CRD is incorrect, or the window selects more nodes than the configured limit.
- `nodeStates` (map): each node's decision for this window, including nodes outside its schedule.
  Keys are node names.
  values are:
  `UnderMaintenance`
  `UnmetTrigger`
  `MaxConcurrencyReached`
  `Unknown` (means evaluation has not completed. It never authorizes maintenance)
  `SelectorMismatch`
  `PastMaintenanceWindow`
  `FutureMaintenanceWindow`
  `InvalidMaintenanceWindow`

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

### What does it look like on my node?

Most of the time:

```
type: `MaintenanceInProgress`
status: False
reason: KumaNotFound
message: "No maintenance window currently authorizes this node"
lastTransitionTime: <time the condition became False>
lastHeartbeatTime: <recent time showing controller liveness>
```

The precise reason a window did not authorize this node is in that window's
`status.nodeStates`, not in this global condition.

During a maintenance:

```
type: `MaintenanceInProgress`
status: True
reason: KumaInProgress
message: "An active maintenance window selecting this node exists"
lastTransitionTime: <time the condition became True>
lastHeartbeatTime: <recent time showing controller liveness>
```

### Heartbeat contract

`lastHeartbeatTime` on kumaco's conditions lets consumers tell a live controller from a dead
one, because a dead controller leaves `True` in place forever.
- The controller refreshes `lastHeartbeatTime` on every condition it owns when it is older
  than `--heartbeat-interval` (default 5m, with jitter), in the same patch as any other change.
- Consumers should treat a `True` condition whose heartbeat is older than 3 intervals
  (15m by default) as **not authorizing new work**.
- In-flight work is the consumer's decision. kured, for example, should finish a reboot it
  already started.

### How does it work?

#### Data structure

A controller only manages the nodes matching its node scope (`--node-selector`, a label
selector; the default, all nodes, is only safe with a single controller). Window selectors
are evaluated within this scope. "Managed nodes" below always means this set. A node leaving
the scope keeps its last conditions: clear them before relabeling it.

We mostly rely on two global mutated maps with locks:
- currentMaintenanceWindows
- globalNodesUnderMaintenance

`currentMaintenanceWindows` maps each window name to a state for every managed node,
using the same values as `status.nodeStates`.
This per-window map is the source for each window's extra condition and status.

`globalNodesUnderMaintenance` maps every managed node to `True` if any window has
`UnderMaintenance` for it, or `False` otherwise.
It is the only source for the shared `MaintenanceInProgress` condition.
It is built from the `currentMaintenanceWindows` map (the per-window maps).
The recommended pattern is to rebuild the `globalNodesUnderMaintenance` at
each change of `currentMaintenanceWindows` map.
Do not update it manually.

#### Reconciliation process

Reminder: In this controller, all the nodes absent from `globalNodesUnderMaintenance`
are considered as "in need of garbage collection".

This means we need to build a world view at container start (or on gaining leadership), to
prevent the first reconcile to garbage collect all the nodes not matched by the "currently
reconciling" maintenance window.

We block/queue the reconciles at this point.
The step to rebuild the world view is to read all MaintenanceWindows
and run the usual reconcile loop _without k8s writes_.
For that, we need to load/evaluate all existing mw (with the same functions as usual
reconcile).

The replay must not forget which nodes are already under maintenance, otherwise admission
would choose again from scratch and, with a low `concurrency`, could revoke nodes that are
mid-maintenance in favour of others. So before evaluating each window, seed
`currentMaintenanceWindows[mw.name][node]=UnderMaintenance` for every node listed as
`UnderMaintenance` in that window's persisted `status.nodeStates`. Those nodes are then
handled as "already in this window" (substep 3.2.1) and counted against its concurrency.
`status.nodeStates` is written before node conditions (step 5 before step 6), so after a
crash it is at least as recent as the nodes.
Rebuild `globalNodesUnderMaintenance` as each window's decisions change so later
windows can see earlier decisions when combining maintenance. Do not publish node
conditions until all windows have been evaluated and the final global map is built.

After this point, both `currentMaintenanceWindow` and `globalNodesUnderMaintenance`
are considered "fresh" (though they might be already outdated), but the nodes
have not been modified from the information we have:
They need garbage collecting before the first reconcile! (as they might have
outdated `MaintenanceInProgress`).

We therefore need one last step for startup: Update the nodes
`MaintenanceInProgress` condition type. For all nodes, we use the
`globalNodesUnderMaintenance[node]` as the boolean to flip the
reason and status of the Condition. In case a transition happens, update the
time of the transition too. (See section "What does it look like on my node?").
The same pass removes every condition on managed nodes whose type starts with the reserved
prefix but is not the `extraConditionType` of an existing window. This cleans up after
windows deleted, or whose type changed, while kumaco was down.

After this startup sequence, we can start reconciling
each MaintenanceWindow separately in the controller.

Case 1: Deletion of a maintenance window.
When a maintenance window is observed as deleted:
1. extract all node names from the mw;
2. delete that mw window's map in `currentMaintenanceWindow` and rebuild the global map as usual
3. loop over the extracted node names. For any node which now is transitioning state
   (based on a check in `globalNodesUnderMaintenance`), update the k8s Condition
   `MaintenanceInProgress`.

Case 2: Every other maintenance window change.
Run a k8s request, get all nodes. I will call this copied list "pristineListOfNodes".
First step - Select nodes:
- Read mw.nodeSelector, use it to run a k8s request (or alternatively, copy the pristineListOfNodes and filter on that list): get all nodes matching the selector. I will call the list of nodes "nodesToProcess" further down.
- If `mw.extraConditionType` has changed, record all nodes which need cleaning + the condition to remove
Any unknown node (=not present in globalNodesUnderMaintenance) needs adding as `currentMaintenanceWindows[mw.name][nodename]=Unknown`.
Then, any node not matching should be updated to `currentMaintenanceWindow[mw.name][nodename]=SelectorMismatch` 
Regenerate the globalNodesUnderMaintenance map if a change happened.
If # of nodes matching exceeds limits, set mw.status.state Invalid and emit an event. Nodes already
`UnderMaintenance` in this window keep that state (in-flight maintenance is never revoked); every other
node is `InvalidMaintenanceWindow`, so nothing new is admitted. Existing members are still released
under the usual rules (schedule, `stopTriggers`) in steps 2-3, but step 4 and new admissions are skipped.
Only go over steps 2-3-4 for nodes that are NOT SelectorMismatch.

Second step - Evaluate schedule:
- The window is `InProgress` if now is between an occurrence's start and its start plus `duration` (regardless of whether there is a future start)
- The window is `Scheduled` if no occurrence is active and a next start > now exists
- The window is `Closed` if no occurrence is active and no future start exists
- The window is `Invalid` if we cannot parse the schedule.
For the `Scheduled`, `Closed`, `Invalid` case: Read/Update `currentMaintenanceWindow` of all matching nodes (found in step 1) to respectively `FutureMaintenanceWindow`, `PastMaintenanceWindow`, and `InvalidMaintenanceWindow`.
In case of change, regenerate the `globalNodesUnderMaintenance` map. Only go over steps 3-4 if `InProgress`.
When the window is not `InProgress`, nodes of this window that were `UnderMaintenance` leave maintenance
(their state becomes the one above).
 
Third step - Iterate over each node 
Before next substeps, we sort the list of all "nodesToProcess" by the node's condition `MaintenanceInProgress`'s last Transition time if it exists.
If not, by node age. More recent first. Ties are broken by node name so that the order is deterministic.

Substep 3.1 - Set node triggers:
If `currentMaintenanceWindows[mw.name][nodename]` is already `UnderMaintenance`, the node is released when ANY of
`mw.stopTriggers` matches (no `stopTriggers`: it is kept, skip to substep 3.2.1).
Otherwise, the node is admitted only if ALL `mw.startTriggers` match and NONE of `mw.stopTriggers` matches.
In the releasing or not-admitted cases, Set `currentMaintenanceWindows[mw.name][nodename]=UnmetTrigger`
(for a node that was `UnderMaintenance`, this frees its slot).
Remove the node from the `nodesToProcess` list.
Continue to next node at substep 3.1.
In case of change, regenerate the `globalNodesUnderMaintenance` map.

Substep 3.2 - Combined mw checking.
The current node, selected by the nodeSelector + matching ALL CEL mw.startTriggers (and no stopTriggers) + `InProgress`, might have _another maintenance window active_ for it.
If a node is _already in another maintenance window_ and the user has decided to _not exclude it_ (this can be done with the help of triggers), we consider the user wants to _combine_ maintenance windows.
Check whether any window's entry for this node is `UnderMaintenance` (read the `globalNodesUnderMaintenance[nodename]` boolean). It can be another maintenance window OR THIS ONE.
If it is *not* the case, move to next node at substep 3.1, leaving the nodesToProcess untouched.

Substep 3.2.1 Check if node is already in this current mw by any chance
Read `currentMaintenanceWindows[mw.name][nodename]`. If it is `UnderMaintenance`, leave it as is.
Remove the node from the `nodesToProcess` list.
Continue to next node at substep 3.1.

Substep 3.2.2 _Can the node (already in a maintenance) be added to this maintenance window or does it exceed our cap of nodes for this maintenance window?_
Extract the `currentlyProcessingNodesCount` value from `currentMaintenanceWindows[mw.name][*]==UnderMaintenance` (we already know you are NOT in it, thanks to 3.2.1)
Extract the `mw.spec.concurrency`.
If `currentlyProcessingCount` >= `spec.concurrency`,
  set `currentMaintenanceWindows[mw.name][nodename]=`MaxConcurrencyReached` and regenerate the `globalNodesUnderMaintenance`.
  Remove the node from the `nodesToProcess` list.
  Continue with the next node at substep 3.1

Else, add the new node to the mix:
  Set `currentMaintenanceWindows[name][nodename]` to `UnderMaintenance` and regenerate the globalNodesUnderMaintenance.
  Remove the node from the `nodesToProcess` list.
  Continue to next node at substep 3.1.

Fourth step - Iterate over each node in reverse order.
The remaining of the `nodesToProcess` are not part of any "combined" maintenance windows. We are talking about "Standalone" mw.
We probably want to have _older nodes_ processed first.
So we iterate over the list `nodesToProcess` in the _reverse order_.
We know that the node cannot be already `UnderMaintenance`, or it would have been caught in substep 3.2.

Substep 4.1 - _Can the node (already in a maintenance) be added to this maintenance window or does it exceed our cap of nodes for this maintenance window?_
This is the same question as 3.2.2, and we proceed _similarly to it_. Just continue at substep 4.1 instead of 3.1.

No nodes should be left to process.
The maintenance window data is ready.

Fifth step - update the mw status.
Update the MaintenanceWindow `status.nodeStates` (before any node write, see startup).

Sixth step - update all the nodes
Loop over `pristineListOfNodes`.
If node.Status.Conditions.["MaintenanceInProgress"].Status for the node != `globalNodesUnderMaintenance[nodename]`, update the node. This can happen upon entering or leaving the mw.
If node.Status.Condition.[mw.spec.extraConditionType].Status for the node != `currentMaintenanceWindows[mw.name][nodename]==UnderMaintenance`, update the node. This can happen upon entering or leaving the mw.
Move to next node.

Warning: In 6th step, when I say update the node, I mean: use server-side apply on `nodes/status`
with the field manager `kumaco`, sending only kumaco-owned condition types, so other reporters'
conditions are preserved and ownership is explicit. Never replace the whole conditions list.
(Check that `NodeStatus.conditions` is a map-type list, keyed by `type`, in all supported Kubernetes versions;
otherwise fall back to a strategic merge patch.)
A write that admits a node (`MaintenanceInProgress` going to `True`) sets `metadata.resourceVersion`
to the version the decision was based on, so a concurrent change to the node (for example the
reboot reporter) rejects the write. Releases and heartbeats are applied without that precondition,
as they are always safe. If a write is rejected or fails, requeue its decision: refetch the node and current windows,
reevaluate at the current time, and rebuild the global decision before another write. Never retry an old boolean after a schedule boundary.
Repeated reconciles should write nothing when conditions and reasons match.
Refresh heartbeat timestamps when due on conditions still owned by the controller (see "Heartbeat contract"). Heartbeat-only node changes will not cause another admission pass (they are filtered).

### Handling MaintenanceWindow conflicts

#### Scheduling conflicts

If two maintenance windows overlap, only one is sufficient to be considered as _under maintenance_.
In other words if you have two MaintenanceWindows A and B applied on all the nodes of a cluster:

A might be in state Closed/Scheduled
B might be in state InProgress

--> All nodes will be considered for _under maintenance_.

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

As the update of the k8s only happen at the end of the reconcile, no concurrency conflicts should happen:
- Each node has its own state in its own mw (including a MaxConcurrencyReached if necessary)
- We reconcile across all nodes across all mw afterwards

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
API view to sync, then reads the current managed nodes and windows, rebuilds both maps
from those inputs, and sweeps node conditions before ordinary reconciliation; it does
not inherit the previous leader's in-memory decisions. A replica stops writing when it
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
3) Set the following CEL `startTriggers` (all must match to admit a node):

```cel
node.status.conditions.exists(c, c.type == 'detector.kured.dev/RebootRequired' && c.status == 'True')
node.status.conditions.exists(c, c.type == 'blockers.kured.dev/NoPodInhibitor' && c.status == 'True')
node.status.conditions.exists(c, c.type == 'blockers.kured.dev/NoAlertInhibitor' && c.status == 'True')
```

This example assumes you are running the blocker reporter, which publishes explicit `NoPodInhibitor` and
`NoAlertInhibitor` conditions.
This component set the value to `True` only when it has checked and found no blocker.
For these triggers, missing, `False`, and `Unknown` do not authorize maintenance.

Also set this `stopTriggers`, so that kured nodes are released once the reboot is done:

```cel
!node.status.conditions.exists(c, c.type == 'detector.kured.dev/RebootRequired' && c.status == 'True')
```

An inhibitor appearing after kured started draining is too late to matter: only `startTriggers` look
at inhibitors, so it does not release the node. `RebootRequired` no longer being `True` (set to
`False`, or removed) after the reboot releases the node and its concurrency slot. While the node reboots,
the stale `True` keeps it under maintenance. Without this `stopTriggers`, the node would keep its
slot until the window ends.

Other triggers may explicitly match a missing or `Unknown` condition, for example:

```cel
!node.status.conditions.exists(c, c.type == 'blockers.kured.dev/NoPodInhibitor')
node.status.conditions.exists(c, c.type == 'blockers.kured.dev/NoPodInhibitor' && c.status == 'Unknown')
```

Each expression above matches only its stated case; it does not override the schedule,
selector, or other triggers. If a reporter publishes a condition only when blocked and
removes it otherwise, absence cannot distinguish "safe" from "not checked yet". The
positive `NoPodInhibitor` trigger above requires the reporter to report readiness.

#### Explanation of interconnection with kured

This controller can not only be used to _define a condition on nodes_,
it can be used as _the brain of maintenances_.

In the kured case, we wanted to have a source of truth to know if that node
was in maintenance to _trigger the rest of the process_
(Drain/Cordon/Reboot/Uncordon), which is done by another controller.

Kured requires kumaco to:
- Check whether the node is in a maintenance windows (core feature of kumaco)
- Check whether the node has other positively matching conditions (core feature of kumaco):
  the node needs `detector.kured.dev/RebootRequired` set to `True` in order to start the Maintenance
  (and set `MaintenanceInProgress`).
- Check that neither a pod nor an alert inhibits the reboot, using the positive conditions shown above.
  With these triggers, an absent or `Unknown` inhibitor report does not authorize maintenance.
  This is also a core feature of kumaco.
- Restrict how many nodes can be under maintenance at the same time for a maintenance.
  This is also a core feature of kumaco, restricting the amount of nodes per maintenance
  window, and bundling the maintenance window together if possible.

Kured then only focuses on setting new conditions/labels/annotations on nodes and rebooting.
Those node changes requeue kumaco reconciliations, which is fine: a reconcile that changes nothing writes nothing.

## Metrics

Metrics report the same decisions as reconciliation. On startup, the controller rebuilds the two in-memory maps from the managed nodes and MaintenanceWindows before exporting their values.

Each controller exports, based on those maps:

- `kumaco_maintenance_window_active{window}`: gauge, 1 while that window's schedule is
  active, 0 otherwise.
- `kumaco_maintenance_window_nodes{window}`: gauge, number of nodes in maintenance.
- `kumaco_node_slot_occupied{node}`: gauge for each managed node, 1 if
  `MaintenanceInProgress` is `True`, 0 otherwise.
  Reflects the content of `globalNodesUnderMaintenance`
- `kumaco_maintenance_window_invalid{window}`: gauge, 1 while the window is `Invalid`.
- `kumaco_reconcile_total{window,result}`: counter of reconciles (`result` is `ok` or `error`).
- `kumaco_reconcile_duration_seconds`: histogram of reconcile duration.
- `kumaco_node_write_total{result}`: counter of node condition writes (`ok`, `conflict`, `error`).
- `kumaco_oldest_heartbeat_age_seconds`: gauge, age of the oldest owned condition heartbeat
  (alert when it exceeds the heartbeat interval by a wide margin).
- `kumaco_leader`: gauge, 1 on the replica currently leading, 0 otherwise.

## Gotchas

A trigger depending on the condition this window changes can oscillate.
For example, requiring MaintenanceInProgress to be absent causes it to alternate between True and False.

The user should not do that. We do not validate it. An administrator can use an admission rule to prohibit self-referential triggers; reading a condition set by a different window can be intentional.

`status.nodeStates` is stored in a single Kubernetes object. Even with numeric values,
node names and JSON overhead can exceed the API's object-size limit. Configure a
maximum number of **managed** nodes per window at controller startup (via CLI or
environment). If it is exceeded, mark the window `Invalid`, emit an event, and admit no new
node. In-flight maintenance is never revoked: nodes already `UnderMaintenance` stay so until
released normally, and the other nodes are `InvalidMaintenanceWindow`. Rather than
silently truncating `status.nodeStates`, publish only its `UnderMaintenance` entries (bounded
by `concurrency`, and needed to seed the state after a restart). This is the exception to
publishing an entry for every managed node.

When a window is deleted, remove its map and recompute `MaintenanceInProgress` for all
managed nodes, even if no windows remain, and remove the window's extra condition from
the nodes. If the deletion (or an `extraConditionType` change) happened while kumaco was
down, the startup sweep removes any condition carrying the reserved prefix that no existing
window owns. Consumers should still apply the heartbeat contract to extra conditions.

## Debugging with kumaco events

kumaco displays actions in its logs, which are intended for the _kumaco deployer_, to debug the controller.

## Security and access rights

Maintenance managers need permission to manage their team's MaintenanceWindows, not
permission to write nodes. Each controller ServiceAccount needs to watch its windows,
read nodes, and update `nodes/status` for its team's nodes. RBAC can restrict writes to
named nodes with `resourceNames`, but not to particular node condition types or to
nodes matching a label. Grant only the node names that controller should manage; a
compromised controller can change *any* status condition on those allowed nodes.
Because maintenance managers choose `extraConditionType`, it must be restricted to the
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
