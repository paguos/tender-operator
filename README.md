# tender

A small Kubernetes operator that supplies apps with Google Cloud resources, declared next to the app's own manifests.

Two kinds today, one controller each in the same binary:

- **`Identity`**: a Google service account for an app, with its key delivered as a Secret in the app's namespace.
- **`Bucket`**: a Cloud Storage bucket, plus access for the Identities it lists.

An app usually has one Identity, and every resource it uses grants access to it. More kinds can follow the same pattern.

```yaml
apiVersion: cloud.paguos.dev/v1alpha1
kind: Identity
metadata:
  name: my-app
  namespace: my-app
---
apiVersion: cloud.paguos.dev/v1alpha1
kind: Bucket
metadata:
  name: files
  namespace: my-app
spec:
  location: EU
  access:
    - identityRef: my-app        # an Identity in the same namespace
      role: objectAdmin          # objectViewer | objectCreator | objectAdmin (default)
```

That gives you:

| What | Name |
|---|---|
| Service account | `<prefix>-<namespace>-<name>-<hash>@<project>.iam.gserviceaccount.com` (shortened to fit 30 chars) |
| Secret `my-app-gcp` | `credentials.json` (the key), `SERVICE_ACCOUNT_EMAIL`, `GCP_PROJECT` |
| Bucket (uniform access, public access prevented) | `<prefix>-<namespace>-<name>`, e.g. `tender-my-app-files` |
| IAM binding on the bucket | `roles/storage.objectAdmin` for the Identity's service account |
| ConfigMap `files-bucket` | `BUCKET_NAME`, `BUCKET_URL`, `GCP_PROJECT` |

```console
$ kubectl get identities -n my-app
NAME     SERVICE ACCOUNT                                                  READY   AGE
my-app   tender-my-app-my-app-1a2b3c4d@my-project.iam.gserviceaccount.com   True    1m
$ kubectl get buckets -n my-app
NAME    BUCKET                READY   AGE
files   tender-my-app-files   True    1m
```

Use it in a Pod:

```yaml
env:
  - name: BUCKET_NAME
    valueFrom: { configMapKeyRef: { name: files-bucket, key: BUCKET_NAME } }
  - name: GOOGLE_APPLICATION_CREDENTIALS
    value: /var/run/secrets/gcp/credentials.json
volumeMounts:
  - { name: gcp, mountPath: /var/run/secrets/gcp, readOnly: true }
# volumes:
#   - name: gcp
#     secret: { secretName: my-app-gcp }
```

## Spec

### Identity

| Field | Default | Notes |
|---|---|---|
| `credentialsSecret` | `<name>-gcp` | Immutable. |

### Bucket

| Field | Default | Notes |
|---|---|---|
| `location` | required | `EU`, `US`, `europe-west3`, ... Immutable. |
| `storageClass` | `STANDARD` | `STANDARD`, `NEARLINE`, `COLDLINE`, `ARCHIVE` |
| `bucketName` | generated | Bucket names are global. Immutable. |
| `deletionPolicy` | `Retain` | `Delete` removes the bucket only once it's empty. A bucket with objects blocks deletion (condition reason `DeletionBlocked`) until you empty it. |
| `access[].identityRef` | | An Identity in the same namespace. References never cross namespaces. |
| `access[].role` | `objectAdmin` | `objectViewer`, `objectCreator` or `objectAdmin` (the `roles/storage.*` roles). One role per Identity. |

## Behaviour

- **Reconcile:** creates what's missing and resets drift (storage class, labels, IAM bindings). Ready objects are rechecked every `--resync` (10m).
- **Access:** a Bucket grants each listed Identity its role and records what it granted in `status.bindings`. Bindings it granted that leave `access` are revoked. Bindings tender didn't grant are never touched. While a referenced Identity is missing or not ready, the bucket still exists but no access changes are made (reasons `IdentityNotFound`, `IdentityNotReady`). The Bucket picks up the change as soon as the Identity becomes ready.
- **Ownership:** buckets carry the labels `managed-by=tender`, `namespace` and `name`. tender refuses to adopt a bucket without them (reason `BucketConflict`), and never changes or deletes one. It also refuses to overwrite a Secret or ConfigMap it doesn't own (`SecretConflict`, `ConfigMapConflict`).
- **Keys:** one key per Identity. The Secret records its ID in the `cloud.paguos.dev/key-id` annotation. If the Secret is deleted or the key is revoked, tender creates a new key, then deletes every other key of that service account. To rotate, delete the Secret.
- **Deleting an Identity** deletes its service account and keys. Its Secret is garbage-collected. Buckets that still reference it go to `IdentityNotFound`.
- **Deleting a Bucket** depends on `deletionPolicy`. `Retain` keeps the bucket and revokes the bindings tender granted. `Delete` removes the bucket once it's empty. The ConfigMap is garbage-collected.

## Install

The operator needs a Google service account of its own:

```bash
PROJECT=my-project
gcloud services enable storage.googleapis.com iam.googleapis.com --project $PROJECT
gcloud iam service-accounts create tender --display-name "tender operator" --project $PROJECT
for role in roles/storage.admin roles/iam.serviceAccountAdmin roles/iam.serviceAccountKeyAdmin; do
  gcloud projects add-iam-policy-binding $PROJECT \
    --member "serviceAccount:tender@$PROJECT.iam.gserviceaccount.com" --role $role
done
gcloud iam service-accounts keys create credentials.json \
  --iam-account tender@$PROJECT.iam.gserviceaccount.com
```

Then install with Kustomize, pinned to a release:

<!-- x-release-please-start-version -->
```yaml
# kustomization.yaml
resources:
  - https://github.com/paguos/tender-operator//config/default?ref=v0.1.0
configMapGenerator:
  - name: tender-config
    namespace: tender-system
    behavior: merge
    literals:
      - project=my-project
      - name-prefix=tender   # make it unique per cluster if clusters share a project
```
<!-- x-release-please-end -->

and provide the key as Secret `tender-gcp-credentials` (key `credentials.json`) in `tender-system`, e.g. with `kubectl create secret generic tender-gcp-credentials -n tender-system --from-file=credentials.json`, or with External Secrets.

Images: `ghcr.io/paguos/tender-operator:<version>` (linux/amd64, linux/arm64, distroless, non-root).

### Flags

| Flag | Env | Default |
|---|---|---|
| `--project` | `TENDER_PROJECT` | required |
| `--name-prefix` | `TENDER_NAME_PREFIX` | `tender` |
| `--resync` | | `10m` |
| `--metrics-bind-address` | | `0` (off) |
| `--health-probe-bind-address` | | `:8081` |
| `--leader-elect` | | `false` |

## Security

The operator's service account can create service accounts and keys anywhere in its project, so it can act as any service account there. **Give tender its own project**, holding only the resources it manages, rather than a project that holds anything more privileged.

Organisations created since 2024 enforce `iam.disableServiceAccountKeyCreation` by default. Keys only work where that policy is off.

## Development

```bash
make generate   # deepcopy, CRD and RBAC from api/ and the +kubebuilder markers
make lint test
make image
```

Tests need no cloud account:

- `internal/cloud/fake` is an in-memory `cloud.Provider` (buckets, objects, IAM bindings, service accounts, keys). The controller tests run against it with controller-runtime's fake client.
- The GCS half of the real provider (`internal/cloud/gcp`) is tested against [fake-gcs-server](https://github.com/fsouza/fake-gcs-server). IAM has no fake server, so that half is only covered through the in-memory provider.

Layout:

```
api/v1alpha1/        API types (one file per kind)
cmd/                 the manager
internal/cloud/      Provider interface, gcp/ implementation, fake/ for tests
internal/controller/ one reconciler per kind
internal/naming/     valid, stable cloud names
config/              CRD, RBAC and Deployment (Kustomize); default/ is the install
```

Adding a kind: add its types under `api/v1alpha1/`, the methods it needs to `cloud.Provider` (and the fake), a reconciler in `internal/controller/`, then `make generate`.

## Releasing

See [docs/releasing.md](docs/releasing.md).
