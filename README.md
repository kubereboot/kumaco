# kumaco - Kubernetes Maintenance Controller

- [kumaco - Kubernetes Maintenance Controller](#Introduction)
- [Behaviour](#Behaviour)
- [Metrics](#metrics)
- [Debugging](#Debugging-with-kumaco-events)
- [Security and access rights](#Security-and-access-rights)
- [Getting Help](#Getting-Help)
- [Trademarks](#trademarks)

This is code is in early alpha.

## Introduction

kumaco (KUbernetes MAintenance COntroller) is a kubernetes controller
watching a "Maintenance" Custom Resource (declaring maintenance windows)
and sets nodes matching criteria under _maintenance_ (by applying the
kubernetes 1.37 standardized `MaintenanceInProgress` on the appropriate nodes).

As of today, kumaco is the most important part of kubereboot (kured) project.
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
- kumaco's maintenance mananager, who has administrative rights to put a node in/out of maintenance, but do not have access rights to nodes themselves.

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
  universally standardized cron expression.

- `duration` (duration): maximum real elapsed time for a maintenance window. In other words: a maintenance window is active if between [startTime, startTime+duration]
- `nodeSelector` (kubernetes labelSelector): the nodes on which the maintenance window is applied to.
- `triggers` (list): A list of CEL expressions that need to ALL apply to mark the node under evaluation as triggered for maintenance
  Triggers can be based on diverse node information: labels, annotations, name, status, and conditions.
- `concurrency` (int): the maximum number of distinct nodes admitted for this maintenance window.
  A node already counted in another maintenance window might be reused for two maintenance at the same time (depends on their triggers). See concurrency conflicts below;
- `extraConditionType` (optional, string). Any eventual extra condition type to set on the node when it goes into / leaves maintenance.
  This can be used for legacy reasons or to track multiple concurrent maintenance that should not overlap their node actions.

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
  - `Invalid`: the input of the CRD is incorrect.
- `nodesUnderMaintenance` (map): This is map containing all the nodes matching the nodeSelector, and their combined _Trigger_ and _concurrency_ state.
  In other words: `nodesUnderMaintenance` has keys equal to node names, and value a string: UnderMaintenance/UnmetTrigger/MaxConcurrencyReached/Unknown

### What does it look like on my node?

Most of the time:

```
type: `MaintenanceInProgress`
status: False
reason: KumaNoMaintenanceWindow
message: "No active maintenance window selecting this node exists"
lastTransitionTime: <date and time of the first poll where no triggers match>
lastHeartbeatTime: <recent time showing controller liveness>
```

If safeguarded:
```
type: `MaintenanceInProgress`
status: False
reason: KumaTriggersMismatch
message: "No active maintenance window selecting this node has all its triggers matching"
lastTransitionTime: <date and time of the first poll where no triggers match>
lastHeartbeatTime: <recent time showing controller liveness>
```

During a maintenance:

```
type: `MaintenanceInProgress`
status: True
reason: KumaEnteredMaintenanceWindow
message: "An active maintenance window selecting this node exists"
lastTransitionTime: <date and time of the first poll where no triggers match>
lastHeartbeatTime: <recent time showing controller liveness>
```

### How does it work?

#### Data structure

We mostly rely on two global mutated maps with locks:
- currentMaintenanceWindows
- globalNodesUnderMaintenance

`currentMaintenanceWindows` is a global map, with maintenance window names as keys (read from the CRD's metadata.name), and as values a map containing NodeMaintenanceInformation (Key = node name, Value = enum UnderMaintenance/UnmetTrigger/MaxConcurrencyReached/Unknown).
In other words: `currentMaintenanceWindows[mw name][node name] gives you the authoritative information about whether the node is UnderMaintenance, has Unmet Trigger, cannot be added to maintenance due to maximum concurrency reached FOR A DETERMINED MAINTENANCE.

`globalNodesUnderMaintenance` is a global map, it contains as keys the nodenames, and as value boolean (True for MaintenanceInProgress, False otherwise).
It is meant as efficient tool to query information ABOUT WHETHER A NODE IS UNDER ANY MAINTENANCE (regardless of the maintenance window).
This is a tool for querying, not writing, information.
This map is regenerated every time there is a change in `currentMaintenanceWindows`.

#### Reconciliation process

At container start, the system will read all the k8s nodes and maintenance windows, in order to establish establish a list of nodes who are currently in maintenance (restore from crash).
This should be blocking all reconciles until the memory is completed refreshed.

We then reconcile each MaintenanceWindow separately.

The queuing (reconciliation of the maintenance windows) should happen:
- On timer, as it might change based on schedule
- On a periodic basis, to guarantee the state is still accurate
- At each node spec change / relevant status change / Condition transition

First step: we query kubernetes API and fetch all the nodes matching the nodeSelector.
We create an in-memory map `currentMaintenanceWindows[name]`.
At this point copy the list of nodes queried from k8s api (to keep it pristine, see why at the end of this section), as the list of nodes to process will be mutated in this reconcile loop.
Known issue: If someone changes the nodeselector to a subset of nodes, then previously selected nodes do not updates.

Any new node is added as `currentMaintenanceWindows[name][nodename]=Unknown`.
Any node NOT MATCHING ANYMORE will be removed from currentMaintenanceWindows[name].
Again, if any change happen, regenerate the map globalNodesUnderMaintenance.

Second step: reading the schedule.
If it is invalid, mark the `status.state` as `Invalid`.
If it is valid but in the past, with no future occurence, defined `status.state` as `Closed`.
To be worth noting: Any Closed/Invalid state deletes the `currentMaintenanceWindows[name]` (which triggers an update of `globalNodesUnderMaintenance` ,setting the nodes as not under maintenance).
Skip to fifth step (update k8s api) and then do the 6th step (node conditions cleanup)

Third step: we iterate to check each node in a recent order.
For that, we sort the list of all nodes by the node's condition `MaintenanceInProgress`'s last Transition time if it exists.
If not, by node age. More recent first.

A) Matching trigger
We set `currentMaintenanceWindows[name][nodename]=UnmetTrigger` for the nodes who do not match the trigger, regardless of whether the node is already active in another maintenance Window or whether it was previously met.
Again, any update should refresh globalNodesUnderMaintenance.
Delete the node from the list of nodes to process.
Go to step 5 and 6.

I repeat: Do not set anything else if the trigger is met. This will be done later.

B) Combining maintenance windows
If a node is _already in another maintenance window_ and the user has decided to _not exclude it_ (this can be done with the help of triggers), we consider the user wants to _combine_ maintenance windows.
To know that, we can simply check if globalNodesUnderMaintenance[nodename] exists and is `true`.
If it does not, we are NOT combining for this node, skip to next step.
We then need to answer this question: _Can the node (already in a maintenance) be added to this maintenance window or does it exceed our cap of nodes for this maintenance window?_

For that, we follow our regular _add node to maintenance process_.

For that we check the map `currentMaintenanceWindows[name]`.
If `currentMaintenanceWindows[name][nodename]` is already `UnderMaintenance`, keep it that way. Delete the node from the list of nodes to process.
If `currentMaintenanceWindows[name][nodename]` is not `UnderMaintenance`, we need to update it if we can (based on concurrency).
Extract the _currentlyProcessingCount_ value from `currentMaintenanceWindows[name]`.
If _currentlyProcessingCount_ < `spec.concurrency`, we can add a new node to the mix: Set `currentMaintenanceWindows[name][nodename]` to `UnderMaintenance`, regenerate the globalNodesUnderMaintenance. Remove the node from the node to process.

Fourth step: Dealing with nodes when NOT combining maintenance windows.
The rest of the nodes have _not been triggered_ by maintenance windows.
We probably want to have _older nodes_ processed first.
So we iterate over the list we got at the beginning of the third step in the _reverse order_.

We know that step 3 has processed unmet, and already under maintenance nodes. 
We only need to follow the regular _add node to maintenance process_.

For the node, we check the map `currentMaintenanceWindows[name]`.
If `currentMaintenanceWindows[name][nodename]` is already `UnderMaintenance`, keep it that way. Delete the node from the list of nodes to process.
If `currentMaintenanceWindows[name][nodename]` is not `UnderMaintenance`, we need to update it if we can (based on concurrency).
Extract the _currentlyProcessingCount_ value from `currentMaintenanceWindows[name]`.
If _currentlyProcessingCount_ < `spec.concurrency`, we can add a new node to the mix: Set `currentMaintenanceWindows[name][nodename]` to `UnderMaintenance`, regenerate the globalNodesUnderMaintenance. Remove the node from the node to process.
Else, _currentlyProcessingCount_ >= `spec.concurrency`. It means the node cannot be put under maintenance: Mark it with MaxConcurrencyReached.

No nodes should be left to process.
The maintenance window data is ready.

Fifth step: Update k8s data for the maintenance window `status`.

The autoritative information about maintenance is now accurate in the currentMaintenanceWindows[name].
Write the mw's `status.nodesUnderMaintenance` with the data from `currentMaintenanceWindows[name]`.

This also means the k8s node objects ARE NOT UP TO DATE with the reconciled data.

Final step: Transition the required kubernetes nodes
We now can reuse the pristine list of nodes, and compare the nodes's following Conditions types
- MaintenanceInProgress
- condition type stored in the mw.`extraConditionType` if any.

Prepare a write if `pristinelist[nodename].status.Conditions.MaintenanceInProgress.Value` or `pristinelist[nodename].status.Condition.<extraconditiontype>.Value` differs from globalNodesUnderMaintenance
(Also check if the reason need updating!). No update happens on Unknown Value.

Post it to k8s api, which may cause a reconcile of the MWs, but should be smartly filtered with the correct predicates.

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

### What does the controller watch?

1. cluster-wide custom resources `MaintenanceWindow` (matching a certain label if configured for it, for example in the case of multiple kumaco deployments);
2. node changes;
3. time-based requeues.

### multi-tenancy while doing self-service is *not an afterthough*

This solves the following use case:
"As a maintenance manager of team X, I want to reboot all my team X nodes but not team Y nodes".

Each team owns a separate, non-overlapping group of nodes and runs one kumaco controller
for that group. The cluster administrator must keep the groups disjoint: two controllers
writing the same node conditions would overwrite each other's decisions. Each controller
has its own concurrency pool.

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
3) Set the following CEL triggers (all must match):

```cel
node.status.conditions.exists(c, c.type == 'detector.kured.dev/RebootRequired' && c.status == 'True')
node.status.conditions.exists(c, c.type == 'blockers.kured.dev/NoPodInhibitor' && c.status == 'True')
node.status.conditions.exists(c, c.type == 'blockers.kured.dev/NoAlertInhibitor' && c.status == 'True')
```

This example assumes you are running the blocker reporter, which publishes explicit `NoPodInhibitor` and
`NoAlertInhibitor` conditions.
This component set the value to `True` only when it has checked and found no blocker.
For these triggers, missing, `False`, and `Unknown` do not authorize maintenance.

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
This causes requeuing on kumaco, but it is fine: we most do events...

## Metrics

Metrics report the same decisions as reconciliation;
the two maps are the record used to rebuild slot occupancy after a restart.

Each controller exports, based on those maps:

- `kumaco_maintenance_window_active{window}`: gauge, 1 while that window's schedule is
  active, 0 otherwise.
- `kumaco_maintenance_window_nodes{window}`: gauge, number of nodes in maintenance.
- `kumaco_node_slot_occupied{node}`: gauge for each managed node, 1 if
  `MaintenanceInProgress` is `True`, 0 otherwise.
  Reflects the content of `globalNodesUnderMaintenance`

## Gochas

A trigger depending on the condition this window changes can oscillate.
For example, requiring MaintenanceInProgress to be absent causes it to alternate between True and False.

The user should not do that.
We do not validate it. If the administrator is afraid of that, it can write an admission rule preventing
the use of MaintenanceInProgress or preventing the use of the extraConditionType in the trigger.

## Debugging with kumaco events

kumaco displays actions in its logs, which are intended for the _kumaco deployer_, to debug the controller.

## Security and access rights

Maintenance managers need permission to manage their team's MaintenanceWindows, not
permission to write nodes. Each controller ServiceAccount needs to watch its windows,
read nodes, and update `nodes/status` for its team's nodes. RBAC can restrict writes to
named nodes with `resourceNames`, but not to particular node condition types or to
nodes matching a label. Grant only the node names that controller should manage; a
compromised controller can change *any* status condition on those allowed nodes.

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
