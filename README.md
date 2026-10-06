# scaffold-operator

Kubebuilder operator that executes `ScaffoldRequest` (`scaffold.taskapp.io/v1alpha1`)
— a one-time render-and-commit of a [`platform-scaffolds`](https://github.com/entr0pian/platform-scaffolds)
template into a component's newly created GitHub repository. See
[`PLATFORM_API_ARCHITECTURE.md`](https://github.com/entr0pian/platform-architecture/blob/main/PLATFORM_API_ARCHITECTURE.md)'s
CREATION EXCEPTION: ScaffoldRequest section for the full resource model this
operator participates in, and `scaffold-operator-plan.md` (in the repo root
of the wider taskapp workspace) for the design history.

## Description

`component-operator`'s `Component` controller creates a `ScaffoldRequest`
once a component's owned `GitHubRepository` is ready, resolving
`componentName`/`repositoryName`/`owner`/`template`/`version` once and
writing them directly into `spec` — the request is fully self-contained.
This controller (`ScaffoldRequestReconciler`) **never reads `Component`**;
everything it needs to execute already lives in `spec`. That's the hard
boundary between "decides WHAT/WHEN" (component-operator) and "executes"
(scaffold-operator).

On each reconcile of a request that isn't yet terminal, the controller:

1. Resolves `<template>/v<version>` against `platform-scaffolds` to an
   immutable commit SHA (recorded as `status.templateRevision` before
   anything else, so provenance survives even a later failure).
2. Fetches `templates/<template>/scaffold.yaml` and validates that every
   parameter it marks `required` has a non-empty value among the three this
   operator supplies (`componentName`, `repositoryName`, `owner`) — a
   `scaffold.yaml` requiring anything else is a contract change this
   renderer doesn't improvise around.
3. Renders `templates/<template>/template/`: `.tpl` files have the suffix
   stripped and exact-name `{{ paramName }}` placeholders substituted
   (never arbitrary `{{ ... }}` evaluation); every other file is copied
   byte-for-byte, so unrelated Helm/GitHub Actions `{{ }}` syntax in the
   scaffold's own output is never touched.
4. Checks the target repository: **empty** → commits; **non-empty with the
   previously recorded `status.commitSHA` still reachable** → treats this as
   crash/status-write-loss recovery and marks `Completed` without writing
   anything new; **non-empty otherwise** → sets `Blocked`
   (`RepositoryNotEmpty`) and stops — this request has no proof it produced
   the existing content, so it never overwrites or guesses.
5. On an empty repository, resolves the real default branch (never a
   hardcoded `main`) and makes **one** atomic commit via the GitHub Git Data
   API (blob → tree → commit → ref), which is the reason this exists as a
   separate operator instead of Crossplane `RepositoryFile` resources (which
   commit once per file).

`Completed` and `Blocked` are both terminal — neither is ever cleared or
retried automatically. A blocked request needs a human to resolve the
target repository's state and then delete/recreate the `ScaffoldRequest`.

### Credentials — the taskapp-platform-scaffolder GitHub App

`--github-auth` picks how the operator authenticates to GitHub (the chart's
`github.auth`). It never falls back from one mode to the other.

- **`app`** (default): the `taskapp-platform-scaffolder` GitHub App
  (Contents and Workflows read/write, installed on all of the account's
  repositories, so a repository Crossplane just created is covered). The
  chart's ExternalSecret copies `appId`, `installationId` and `privateKey`
  from Secrets Manager (`taskapp/platform/scaffolder-github-app`, owned by
  `bootstrap-cluster/terraform/management-eks`) into a Secret in the
  operator's namespace, readable through a `Role` scoped to `get` on that
  one name. Each request gets two short-lived installation tokens, both
  narrower than the App: one that can only read `platform-scaffolds`, and
  one that can only write contents and workflows to the request's target
  repository. Workflows write is required because GitHub rejects any App
  token that creates `.github/workflows/*`, which the `golang-service`
  scaffold does, without it. Scaffold commits are authored by
  `taskapp-platform-scaffolder[bot]`.
- **`pat`**: the shared `crossplane-github-credentials` Secret
  (`crossplane-system` namespace, single `credentials` key holding
  `{"token":"...","owner":"..."}`), read via a direct cross-namespace
  `client.Get`. The chart only renders the `crossplane-secret-*` Role and
  RoleBinding for this mode. It exists for clusters without the App: kind
  in CI (`test-chart.yml`) and the kustomize deploy used by e2e
  (`config/manager`), neither of which has External Secrets.

### Not yet deployed

Like `component-operator`, this operator is scaffolded and buildable but not
wired into the deployment catalog (`application-repositories/catalog|infra/scaffold-operator/*`)
or ArgoCD yet. The `chart/templates/rbac/crossplane-secret-*.yaml` pair also
needs to be applied against whichever cluster this operator ends up running
on — cross-namespace RBAC that intentionally lives outside the raw
`config/rbac` kustomize tree (kustomize's blanket `namespace:` transform in
`config/default` would otherwise force these into the operator's own
namespace instead of `crossplane-system`); the Helm chart is therefore the
deployment artifact of record for this operator, matching how it already
carries the CRD.

**Image registry note**: `.github/workflows/docker-publish.yml` pushes to
`ghcr.io/entr0pian/scaffold-operator` (GHCR), not Docker Hub — unlike
`component-operator`'s image, which pulls today with no `imagePullSecrets`
because Docker Hub repos default to public. A package pushed to GHCR via
`GITHUB_TOKEN` defaults to **private** regardless of the source repo's
visibility, which would break pulling on a real cluster until either the
package is flipped to public in GitHub's package settings, or
`manager.imagePullSecrets` (already a supported chart value, currently
unset) is wired up with a GHCR pull credential. **Verify package visibility
after the first push, before wiring this into `application-repositories`.**

## Getting Started

### Prerequisites
- go version v1.24.6+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/scaffold-operator:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/scaffold-operator:tag
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

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/scaffold-operator:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/scaffold-operator/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

