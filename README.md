# kumaco - Kubernetes Maintenance Controller

- [kumaco - Kubernetes Maintenance Controller](#Introduction)
- [Behaviour](#Behaviour)
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
- startTime (the scheduled start of the maintenance window, according to the cron standard)
- duration (the duration of the maintenance window from the startTime)
- nodeSelector (the nodes on which the maintenance window is applied to. A kubernetes labelSelector)
- trigger (the list CEL expressions that need to ALL apply to mark the node under evaluation under maintenance)
  Triggers can be based on diverse node information: labels, annotations, name, status, and conditions.
- concurrency (the maximum amount of nodes that can be set under maintenance at the same time)

The MaintenanceWindow CRD's status contains:
- `state` is a summarized result, determined based on spec and conditions.
  State is a practical way for users to check the overall state,
  rather than checking all the conditions independently.
  This is particularly useful when the maintenance manager sets complex CEL rules.
  `state` can be:
  - `InProgress`: self-explanatory :)
  - `Scheduled`: When the cron has a possible _future_ start
  - `Closed`: When the cron expression has no possible _future_ start
  - `Invalid`: When the input of the CRD is incorrect.

### Handling MaintenanceWindow conflicts

#### Scheduling conflicts

If two maintenance windows overlap, only one is required to set the node's `kuma.kured.dev/InWindow`.
In other words if you have two MaintenanceWindows A and B applied on all the nodes of a cluster:

A might be in state Closed/Sheduled
B might be in state InProgress

--> All nodes will have a condition like this:

```
type: `kuma.kured.dev/InWindow`
status: True
reason: KumacoEnteredMaintenance
message: "Under at least one maintenance window"
lastTransitionTime: <time of the Maintenance Window start>
lastHeatbeatTime: <recent time showing controller liveness>
```

(logs or events can help figure out which maintenance window is flipping the condition, metrics
can highlight all the open MaintenanceWindows).

At the opposite, when no maintenance window is valid:

```
type: `kuma.kured.dev/InWindow`
status: False
reason: KumacoNoMaintenance
message: "No maintenance in progress"
lastTransitionTime: <date and time of the first poll where no maintenance is remaining>
lastHeatbeatTime: <recent time showing controller liveness>
```

#### Triggering conflicts

If two maintenance windows overlap, only one is required to set the node's `kuma.kured.dev/Triggered`.

Let's take this example:

The MaintenanceWindow A requests for all nodes to have the following two conditions:
- `kuma.kured.dev/InWindow` set to true
- `detector.kured.dev/RebootRequired` set to true

The MaintenanceWindow B requests for all nodes to have this condition:
- `kuma.kured.dev/InWindow` set to true

Then, because any MaintenanceWindow is _sufficient_ to set the `kuma.kured.dev/Triggered`, 
all nodes will have the following condition:

```
type: `kuma.kured.dev/Triggered`
status: True
reason: KumacoTriggersMatched
message: "Triggered by at least one maintenance window"
lastTransitionTime: <time of the trigger successfully matching>
lastHeatbeatTime: <recent time showing controller liveness>
```

At the opposite, when 100% of the triggers of all Maintenance Windows do not apply on the node:

```
type: `kuma.kured.dev/Triggered`
status: False
reason: KumacoTriggersMismatch
message: "No maintenance window has all its triggers matching for this node"
lastTransitionTime: <date and time of the first poll where no maintenance is remaining>
lastHeatbeatTime: <recent time showing controller liveness>
```

#### nodeSelector and trigger conflicts

The nodeSelector has _precedence_ over trigger.

Let's take the following scenarios:

First, a MaintenanceWindow has a nodeSelector for all nodes, with a trigger matching node.name == "node1".
In this case, the MaintenanceWindow.Triggered True will be set on node1, False on all other nodes.
Only node1 will have the resulting `MaintenanceInProgress` condition.

Second, a MaintenanceWindow has a selector to only match node1, with a trigger matching all nodes.
In this case, the MaintenanceWindow.Triggered True will be set on node1.
This condition won't be set on all other nodes!
Only node1 will have the resulting `MaintenanceInProgress` condition, as the condition requires both the trigger and the InWindow to be set to True.

Third, a MaintenanceWindow has a selector to match node1, with a trigger matching only node2.
In this case, the MaintenanceWindow.Triggered is False to node 1.
This condition won't be set on other nodes.
No node has the resulting `MaintenanceInProgress` set to True.

#### concurrency conflicts

As concurrency is aiming to be the _maximum number of nodes that can be in maintenance at a single point_, this spirit is followed when multiple maintenance windows are concurrent.

In other words: In a scenario where two maintenance windows have different concurrencies values, the Maximum concurrency will be taken.

This allows for faster maintenance, and not restrict a maintenance window size in progress.
To reduce a maintenance window in progress, adapt the nodeSelector or the trigger (which will recompute all the maintenance windows).

Side note: The depending controllers might not react properly to rapid changes of `MaintenanceInProgress` values.
Check with the other controllers about their expected behaviours.

### What does the controller watch?

1. cluster-wide custom resources `MaintenanceWindow` (matching a certain label if configured for it, for example in the case of multiple kumaco deployments);
2. node changes;
3. time-based requeues.

### multi-tenancy while doing self-service is *not an afterthough*

This solves the following use case:
"As a maintenance manager of team X, I want to reboot all my team X nodes but not team Y nodes".

#### The harder approach: the service-account limitations

If you have a kubernetes cluster used by different teams, you can do this by running
n deployments of kumaco, each of them with their own serviceAccount.

You can then grant rights of the serviceAccount to a predefined set of node objects, preventing
unauthorized access to nodes.

In this case, kumaco might actually fail to setup conditions for nodes on which it has no access,
proving isolation.

#### The softer approach: the controller watch label

If you have a kubernetes cluster used by different teams, you can do this by running
n deployments of kumaco, each of them with their watcher label.

Kumaco will only watch for CRD matching the label configured for them.

In other words, the administrator has configured the kumaco controller deployment for team A
to only watching for MaintenanceWindow CRDs matching the label "MaintenanceWindowTeam: A".
The administrator has configured another kumaco controller deployment, for team B, only
watching for the MaintenanceWindow CRD matching the label "MaintenanceWindowTeam: B".

To guarantee multi-tenancy, the administrator has to define an admission policy (with tools like
kubewarden, kyverno, or OPA) tying the identity of the user with:
- its team 
- the applied label
- the nodeSelector

### How kured relies on kumaco, and how to configure your maintenance window for kured:

Basic configuration for kured is the following:

1) Set the maintenance window time
2) Set the nodeSelector
3) Set the following trigger:

- node.Conditions[].type == "detector.kured.dev/RebootRequired" == true
- node.Conditions[].type == "blockers.kured.dev/NoPodInhibitor" == true
- node.Conditions[].type == "blockers.kured.dev/NoAlertInhibitor" == true

#### Explanation

This controller can not only be used to _define a condition on nodes_,
it can be used as _the brain of maintenances_.

In the kured case, we wanted to have a source of truth to know if that node
was in maintenance to _trigger the rest of the process_
(Drain/Cordon/Reboot/Uncordon), which is done by another controller.

Kured requires kumaco to:
- Check whether the node is in a maintenance windows (core feature of kumaco)
- Check whether the node has other positively matching Conditions: the node needs to be rebooted (=have the condition `kured.dev/RebootRequired`), on top of being in a maintenance window, to have the `MaintenanceInProgress` condition.
- Check whether the node has _blocking_ conditions preventing the `MaintenanceInProgress`. For example, kured has a `RebootInhibitors` controller to guarantee the reboot will not happen if some alert is triggered or some pod is present on the node.
  This `RebootInhibitors` sets _blocking_ condition when inhibited, cleans it up when not inhibited anymore.
- Guarantee a maximum of n concurrent `MaintenanceInProgress`. kumaco is required to check all nodes positively matching, removing the negatively matching ones, then select the results until the concurrency ceiling is reached.

## Debugging with kumaco events

kumaco displays actions in its logs, which are intended for the _kumaco deployer_, to debug the controller.

## Security and access rights

The controller ServiceAccount must be able to:
- Watch its CRD
- Apply a _status_ change to nodes and read all nodes

This also means the privileges are restricted.

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
