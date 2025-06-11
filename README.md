# Datum GCP Infrastructure Provider

This provider manages resources in GCP as a result of interpreting workload and
network related API entities defined by users.

The primary APIs driving resource creation are defined in [workload-operator][workload-operator]
and [network-services-operator][network-services-operator].

[workload-operator]: https://github.com/datum-cloud/workload-operator
[network-services-operator]: https://github.com/datum-cloud/network-services-operator

## Documentation

Documentation will be available at [docs.datum.net](https://docs.datum.net/)
shortly.

### Design Notes

#### Instances

Currently this provider leverages [GCP Managed Instance Groups][gcp-migs] to
manage instances within GCP. A future update will move toward more direct
instance control, as MIG resources and entities such as templates that are
required to use them take a considerably longer time to interact with than
direct VM instance control.

[gcp-migs]: https://cloud.google.com/compute/docs/instance-groups#managed_instance_groups

## Getting Started

### Prerequisites

- go version v1.23.0+
- docker version 17.03+.
- kubectl version v1.31.0+.
- Access to a Kubernetes v1.31.0+ cluster.

This provider makes use of [Crossplane][crossplane] to manage resources in GCP.
It is expected that Crossplane and the following Crossplane GCP providers have
been installed in the downstream cluster:

- [provider-gcp-cloudplatform](https://marketplace.upbound.io/providers/upbound/provider-gcp-cloudplatform)
- [provider-gcp-compute](https://marketplace.upbound.io/providers/upbound/provider-gcp-compute)
- [provider-gcp-secretmanager](https://marketplace.upbound.io/providers/upbound/provider-gcp-secretmanager)

[crossplane]: https://crossplane.io/

### To Deploy on the cluster

**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/tmp:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don't work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/tmp:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall

**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

<!-- ## Contributing -->

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)
