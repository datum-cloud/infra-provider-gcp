# Datum GCP Infrastructure Provider

> [!CAUTION]
> This operator is currently in a POC phase. The POC integration branch will
> be orphaned and separate PRs opened for discrete components (APIs, controllers,
> etc) as they mature.

This provider interprets workload related entities and provisions resources to
satisfy workload requirements in GCP.

## Prerequisites

This provider makes use of the [GCP Config Connector][k8s-config-connector]
project to manage resources in GCP. It is expected that the config connector
and associated CRDs have been installed in the cluster.

[k8s-config-connector]: https://github.com/GoogleCloudPlatform/k8s-config-connector

## Design Notes

### Instances

Currently this provider leverages [GCP Managed Instance Groups][gcp-migs] to
manage instances within GCP. A future update will move toward more direct
instance control, as MIG resources and entities such as templates that are
required to use them take a considerably longer time to interact with than
direct VM instance control.

### TCP Gateways

> [!IMPORTANT]
> The controller for this feature is currently disabled as it assumes a workload
> which is deployed to a single project. This will be updated in the future.

TCP gateways for a Workload is provisioned as a global external TCP network load
balancer in GCP. An anycast address is provisioned which is unique to the
workload, and backend services are connected to instance groups.

Similar to the instance group manager, these entities take a considerable amount
of time to provision and become usable. As we move forward to Datum powered LB
capabilities, the use of these services will be removed.

[gcp-migs]: https://cloud.google.com/compute/docs/instance-groups#managed_instance_groups
