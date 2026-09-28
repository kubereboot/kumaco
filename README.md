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
watching ConfigMaps for declaring maintenance windows
and reporting whether a node _is_, or _is not_ currently matching
a maintenance window through node conditions.

On top of knowing whether a node is `UnderMaintenance`, an advanced feature
is to set the `MaintenanceInProgress` Condition (standardized in kubernetes 1.37),
_based on other node information_ (labels, pods, status, conditions).
Depending on the uptake of the kumaco project, we might want to split the advanced
orchestration of maintenance/conditions into another controller.

As of today, kumaco is part of kubereboot (kured) project.
In kured v2 architecture:
- kumaco is the _brain of the orchestration_ 
- [node-reboot-required-reporter](https://github.com/kubereboot/node-reboot-required-reporter) detects whether a reboot is required and mark the node as such (it is running on all nodes).
- [reboot-inhibitors-reporter](https://github.com/kubereboot/reboot-inhibitors-reporter) detects whether a blocker pod is present on nodes or a prometheus alert is present. This could _as well be a node problem detector_ information (it is not running on all nodes).
- [kured](https://github.com/kubereboot/kured) drains and restarts the nodes (it is running on all nodes, but with different privileges as node-reboot-required-reporter)

## Behaviour

### Support different personas

kumaco sets the `kured.dev/UnderMaintenance` _Condition_ on nodes matching the maintenance
windows declared by the user.

This can intentionally be split into multiple namespaces, so that a cluster administrator
can delegate the feature to "mark a node for maintenance" to another user(s).

In this documentation, we will therefore use two different personas:

- The "kumaco deployer", which deploys kumaco into the cluster
- The "maintenance manager", which configures configmaps in the cluster
to put some nodes in/out of maintenance.

Those roles can be merged in a single person/serviceAccount, but the documentation
splits the roles explicitly.

### ConfigMap vs CRD

We believe that maintenance windows should be less intrusive as possible, and therefore
not require CRDs.

Maintenance software should be deployed easily and should not need high privileges.

In other words, you do not need as much access to deploy kumaco
as you would need for other operators. This is *intentional*.

This also means:
- you can deploy kumaco multiple times in the same cluster
- different versions of kumaco can run at the same time
- the maintenance of nodes can be spread into multiple kumacos (for example: one kumaco with
  a serviceAccount only managing nodes in a region `A`, while another kumaco will manage
  nodes in a region `B`.

The absence of a `MaintenanceWindow` CRD has also the following downsides:
- We do not have a way to validate a MaintenanceWindow configuration (at admission).
  This means we must validate it at runtime.
- We do not have a handy resource to _display the state of a maintenance_.
  This means we need to display the maintenance events as _kubernetes events_
  and observe their results in node themselves.
- We cannot use RBAC to restrict access to the `MaintenanceWindow`. This is okay,
  as one could create a dedicated namespace instead.

### ConfigMap structure

This is expected in each configmap's Data:
- startTime (the schedule, according to robfig's cron.ParseStandard - v3). It can be in different formats, check it out!
- duration (the duration of the maintenance window, formatted to golang's time.ParseDuration)
- nodeSelector (the nodes on which the maintenance window is applied to, a kubernetes labelSelector)

### Caution: Rough edges upon editing maintenance window!

The current maintenance windows are based on a start timer and a duration.
The current implementation does not have an interface to allow an easy _edit_ and track changes.
Therefore, current implementation take the assumption that maintenance windows are _immutable_.

This means that a change of the maintenance window will result in a _restart_ of the
controller. This is voluntary! It makes it easy to "regenerate the state of the world" based
on the declared data.

In order to do that, all configmaps' data (not metadata) will be hashed and stored in the controller.

This also means you should NOT have thousands of maintenance windows and should regularly cleanup
the one-shot maintenance windows you have created (or just update the ones you have).

### Why not setting `MaintenancePlanned` on nodes?

As this project is intended to work for kured (but can be used elsewhere),
there was little point to declare `MaintenancePlanned`. It would mean that
the nodes are `always` in MaintenancePlanned.

As of today, there was no demand for `MaintenancePlanned`.

### Advanced feature: Set `MaintenanceInProgress` based on other information.

The goal of this controller was not only _define a condition on nodes_, it was
also _be the brain of maintenances_.

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

kumaco displays actions in its logs, which are intended for the _kumaco deployer_.

Next to this, kumaco also reports _events_ to the watched namespaces.
These events are meant for the _maintenance manager_.

These events will appear:
- A config map is loaded
- A config map in ignored (invalid)
- A maintenance window starts
- A maintenance window has ended

## Security and access rights

The controller ServiceAccount must be able to:
- Watch configmaps in the watching namespaces
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
