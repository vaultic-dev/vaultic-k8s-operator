# k8s-operator (Vaultic)

A Kubernetes operator that materializes Vaultic environments as native `v1.Secret` resources (`Opaque`)
and Vaultic certificates as `kubernetes.io/tls` Secrets, refreshed automatically on an interval.

Built with [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime), following
standard kubebuilder project conventions (`controller-gen` is used for CRD YAML and `DeepCopyObject`
generation). Separate Go module outside the pnpm workspace, like `terraform-provider-vaultic/`.

## Compatibility

`VaulticSecret` works against any Vaultic server. `VaulticCertificate` requires the full Vaultic
product's Certificate Manager and a service token created with Certificate Manager access — it
isn't implemented in the open-source, self-hosted `open-vaultic` server, so every
`VaulticCertificate` reconcile against `open-vaultic` fails with a 404.

Not sure whether your server is the full Vaultic product or self-hosted `open-vaultic`? Both
expose an unauthenticated `GET /version`:

```bash
curl -s "$SERVER_URL/version"
# {"product":"vaultic","version":"1.4.2"}       -> full Vaultic: both CRDs work
# {"product":"open-vaultic","version":"0.1.0"}  -> self-hosted OSS: VaulticSecret only
```

`open-vaultic` is also single-workspace: it only accepts `workspace: default` in the CR spec and
returns 404 for any other value.

## Custom Resources

### 1. `VaulticSecret` (Secrets Manager)

Materializes fully resolved environment secrets (inheritance + `${...}` references) as an `Opaque` Kubernetes `Secret`:

```yaml
apiVersion: vaultic.dev/v1
kind: VaulticSecret
metadata:
  name: backend-api-production
spec:
  serverURL: http://vaultic-server.default.svc.cluster.local:4000
  workspace: acme   # use "default" against a self-hosted open-vaultic server
  project: backend-api
  environment: production
  tokenSecretRef:
    name: vaultic-token   # an existing Secret holding a Vaultic service token
    key: token
  targetSecretName: backend-api-production   # the v1.Secret this controller creates/updates
  refreshIntervalSeconds: 60
```

### 2. `VaulticCertificate` (Certificate Manager)

Requires the full Vaultic product and a service token with Certificate Manager access — see
[Compatibility](#compatibility) above. Materializes a Vaultic certificate and its private key as
a standard `kubernetes.io/tls` Kubernetes `Secret`:

```yaml
apiVersion: vaultic.dev/v1
kind: VaulticCertificate
metadata:
  name: backend-api-tls
spec:
  serverURL: http://vaultic-server.default.svc.cluster.local:4000
  workspace: acme
  application: backend-api
  commonName: api.internal
  tokenSecretRef:
    name: vaultic-token
    key: token
  targetSecretName: backend-api-tls
  refreshIntervalSeconds: 60
```

See `config/samples/` for complete examples including token Secret definitions.

## What the controllers do

### VaulticSecret Reconcile
1. Reads the service token from `spec.tokenSecretRef` (same namespace as the `VaulticSecret`; the token is never stored inline in the CR spec).
2. Calls `GET /workspaces/:ws/projects/:proj/environments/:env/export` on the Vaultic server — the same fully-resolved key/value map `vaultic export` and `vaultic run` use.
3. Creates or updates a native `v1.Secret` named `spec.targetSecretName` (defaults to the CR's own name) with that data, owned by the `VaulticSecret` via `OwnerReference` — so deleting the CR garbage-collects the Secret automatically with no finalizers required. Stale keys removed on the Vaultic side are pruned cleanly from the materialized Kubernetes Secret.
4. Sets a standard `Ready` condition with `ObservedGeneration` and `status.syncedKeyCount` on the CR.
5. Requeues itself after `spec.refreshIntervalSeconds` (default 60, min 5) — enabling auto-refresh even with zero Kubernetes-side events. On fetch or apply errors, it returns the error so controller-runtime's workqueue rate limiter retries with exponential backoff (fast retry for a transient blip, without hammering Vaultic during a sustained outage).

### VaulticCertificate Reconcile
1. Reads the service token from `spec.tokenSecretRef`.
2. Calls `GET /workspaces/:ws/certificate-manager/applications/:app/certificates/:cn/fetch` on the Vaultic server.
3. Creates or updates a native `kubernetes.io/tls` `v1.Secret` with `tls.crt` (certificate + chain) and `tls.key` (private key), owned by the `VaulticCertificate` via `OwnerReference`.
4. Sets standard `Ready` condition with `ObservedGeneration` and `status.version` on the CR.
5. Requeues periodically to pick up server-side certificate rotations and renewals automatically.

Single controller manager process by default (`replicas: 1` in `config/manager/deployment.yaml`).
Pass `--leader-elect` to run more than one replica — the ClusterRole already includes the
`coordination.k8s.io`/`leases` permissions leader election needs.

The manager serves metrics over HTTPS on `:8080` by default, authenticating and authorizing
scrapers via the Kubernetes API server (`--metrics-secure=false` to fall back to plain HTTP).
Bind a scraper's ServiceAccount to the `vaultic-operator-metrics-reader` ClusterRole
(`config/rbac/metrics_reader_role.yaml`) to allow it to read `/metrics`.

## Building & Testing

```bash
cd k8s-operator

make fmt vet test build   # gofmt check, vet, unit tests, binary build
make docker-build          # container image (vaultic/k8s-operator:dev)
```

Equivalent raw commands (what the Makefile wraps) are in [.github/workflows/ci.yml](.github/workflows/ci.yml),
which runs on every push/PR.

The live-server integration test (`internal/controller/live_server_test.go`) is skipped unless
`VAULTIC_LIVE_TOKEN` is set to a service token for a Vaultic instance running on `localhost:4000` —
never hardcode a real token in that file; it's a live credential and would end up committed to git
history.

## Deploying into a Kubernetes cluster

### Production (a released, published image)

`config/manager/deployment.yaml`'s checked-in `image:` points at a [released](#releases) GHCR
tag — bump it to whichever version you want to run before applying.

```bash
# 1. Apply CRDs
kubectl apply -f config/crd/

# 2. RBAC + namespace
kubectl apply -f config/manager/namespace.yaml
kubectl apply -f config/rbac/

# 3. Deploy operator (uses the image pinned in config/manager/deployment.yaml)
kubectl apply -f config/manager/deployment.yaml

# 4. Apply samples
kubectl apply -f config/samples/vaultic_v1_vaulticsecret.yaml
kubectl apply -f config/samples/vaultic_v1_vaulticcertificate.yaml
```

`make install` / `make deploy` (and their `uninstall`/`undeploy` counterparts) wrap steps 1–3;
`make deploy` defaults to `IMG`'s value (`vaultic/k8s-operator:dev` — see below) rather than the
file's checked-in image, so pass `IMG` explicitly for a production deploy through `make`:

```bash
IMG=ghcr.io/vaultic-dev/vaultic-k8s-operator:v0.1.0 make deploy
```

### Local development (`:dev`, unpublished, no registry)

Unchanged from before there was a registry — the local `:dev` tag never leaves your machine:

```bash
make docker-build                                          # builds vaultic/k8s-operator:dev
kind load docker-image vaultic/k8s-operator:dev             # kind
# or: minikube image load vaultic/k8s-operator:dev          # minikube
make deploy                                                 # applies with IMG=vaultic/k8s-operator:dev (the default)
```

Or skip the image entirely and run the manager as a plain local process against whatever cluster
your kubeconfig points at:

```bash
make run   # or: go run ./cmd
```

### Production notes

- **RBAC scope**: the ClusterRole grants `secrets` create/get/list/watch/update/patch/delete
  cluster-wide (not just in namespaces where `VaulticSecret`/`VaulticCertificate` CRs exist),
  because those CRDs are namespaced and can be created in any namespace. This is the standard
  pattern for this class of operator (comparable to `external-secrets`/`cert-manager`), but it
  means RBAC on *who can create `VaulticSecret`/`VaulticCertificate` objects* is doing real
  security work — restrict it per-namespace via your own `Role`/`RoleBinding`s if the cluster is
  multi-tenant.
- **`spec.serverURL`** is fully controlled by whoever can create these CRs (only validated as a
  well-formed `http(s)://` URL). Anyone with permission to create a `VaulticSecret`/
  `VaulticCertificate` can point it at an arbitrary host, so treat CR-creation RBAC as sensitive,
  and prefer `https://` server URLs to avoid sending the bearer token in cleartext.

## Releases

Released images are published to **[GHCR](https://github.com/vaultic-dev/vaultic-k8s-operator/pkgs/container/vaultic-k8s-operator)**:

```
ghcr.io/vaultic-dev/vaultic-k8s-operator:<version>
```

```bash
docker pull ghcr.io/vaultic-dev/vaultic-k8s-operator:v0.1.0
```

Each release publishes three tags built from the same image:

| Tag | Meaning |
|---|---|
| `vX.Y.Z` | The exact semantic-version tag that was pushed — immutable by convention, never re-pushed. |
| `sha-<shortsha>` | A second, independently-immutable reference tied to the exact commit, regardless of whether the version tag is ever moved. |
| `latest` | Only published for a stable `vX.Y.Z` tag (no prerelease suffix) — points at the most recently released stable version. |

### How a release is built

[`.github/workflows/release.yml`](.github/workflows/release.yml) builds the image with Docker
Buildx and pushes it to GHCR, authenticating with the Actions-provided `GITHUB_TOKEN` (scoped to
`packages: write`/`contents: write` for that job only — no registry credentials are stored in the
repo). It runs `gofmt`/`vet`/`build`/`test` first and never tags or publishes on top of a failing
build.

**Automatic — merge a PR into `main` with the `release` label.** Once tests pass against the
merge commit, a patch-bumped tag is computed from the latest existing `vX.Y.Z` tag (`v0.1.0` if
none exist yet) and pushed automatically, then that version is built, published to GHCR, and
turned into a GitHub Release with generated notes. A PR merged *without* the `release` label
doesn't release anything — add the label before (or at) merge time to ship that change.

**Manual override — for a deliberate minor/major bump or a prerelease.** Push a `vX.Y.Z` tag
yourself and the same workflow builds and publishes exactly that tag instead of computing one:

```bash
git checkout main
git pull
git tag v1.0.0
git push origin v1.0.0
```

The next automatic release after a manual tag continues from it (e.g. a manual `v1.0.0` is
followed by an automatic `v1.0.1` the next time a `release`-labeled PR is merged).

Local development never touches GHCR: `vaultic/k8s-operator:dev` (built with `make docker-build`)
stays a throwaway, unpublished tag loaded directly into a local kind/minikube cluster — see
[Local development](#local-development-dev-unpublished-no-registry) above.

There's no Helm chart in this repo (plain YAML manifests under `config/`) — the GHCR image is
consumed by referencing it in `config/manager/deployment.yaml` as shown above.

## Project Layout

```
cmd/main.go                                        manager entrypoint
api/v1/vaulticsecret_types.go                      VaulticSecret CRD Go types
api/v1/vaulticcertificate_types.go                 VaulticCertificate CRD Go types
api/v1/zz_generated.deepcopy.go                     generated DeepCopy methods
internal/controller/vaulticsecret_controller.go    VaulticSecret Reconcile loop
internal/controller/vaulticcertificate_controller.go VaulticCertificate Reconcile loop
config/crd/                                        CRD manifests
config/rbac/                                        ServiceAccount, ClusterRole, ClusterRoleBinding(s)
config/manager/                                    Namespace + Deployment for operator
config/samples/                                    Example CR and Secret manifests
Dockerfile                                         multi-stage build -> distroless static image
Makefile                                            fmt/vet/test/build/docker-build/deploy targets
.github/workflows/ci.yml                            CI: fmt, vet, build, test, docker build
.github/workflows/release.yml                       on vX.Y.Z tag push: build + publish to GHCR
```
