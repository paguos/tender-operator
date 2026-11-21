# tender

A small Kubernetes operator that supplies apps with Google Cloud resources, declared next to the app's own manifests.

Today it manages one kind, `Bucket`: a Cloud Storage bucket plus a service account that can use it, with a key delivered as a Secret in the app's namespace. More kinds (one controller each, same binary) can follow.

```yaml
apiVersion: cloud.paguos.dev/v1alpha1
kind: Bucket
metadata:
  name: files
  namespace: my-app
spec:
  location: EU
```

That gives you:

| What | Name |
|---|---|
| Bucket (uniform access, public access prevented) | `<prefix>-<namespace>-<name>`, e.g. `tender-my-app-files` |
| Service account with `roles/storage.objectAdmin` on that bucket only | `<prefix>-<namespace>-<name>-<hash>@<project>.iam.gserviceaccount.com` (shortened to fit 30 chars) |
| Secret `files-gcs` in `my-app` | `credentials.json` (the key), `BUCKET_NAME`, `GCP_PROJECT` |

```console
$ kubectl get buckets -n my-app
NAME    BUCKET                READY   AGE
files   tender-my-app-files   True    1m
```

Use it in a Pod:

```yaml
env:
  - name: BUCKET_NAME
    valueFrom: { secretKeyRef: { name: files-gcs, key: BUCKET_NAME } }
  - name: GOOGLE_APPLICATION_CREDENTIALS
    value: /var/run/secrets/gcs/credentials.json
volumeMounts:
  - { name: gcs, mountPath: /var/run/secrets/gcs, readOnly: true }
# volumes:
#   - name: gcs
#     secret: { secretName: files-gcs }
```

## Spec

| Field | Default | Notes |
|---|---|---|
| `location` | required | `EU`, `US`, `europe-west3`, ... Immutable. |
| `storageClass` | `STANDARD` | `STANDARD`, `NEARLINE`, `COLDLINE`, `ARCHIVE` |
| `bucketName` | generated | Bucket names are global. Immutable. |
| `deletionPolicy` | `Retain` | `Delete` removes the bucket only once it's empty. A bucket with objects blocks deletion (condition reason `DeletionBlocked`) until you empty it. |
| `credentialsSecret` | `<name>-gcs` | Immutable. |

## Behaviour

- **Reconcile:** creates what's missing and resets drift (storage class, labels, IAM binding). Ready buckets are rechecked every `--resync` (10m).
- **Ownership:** buckets carry the labels `managed-by=tender`, `namespace` and `name`. tender refuses to adopt a bucket without them (reason `BucketConflict`), and never deletes one. It also refuses to overwrite a Secret it doesn't own (reason `SecretConflict`).
- **Keys:** one key per Bucket. The Secret records its ID in the `cloud.paguos.dev/key-id` annotation. If the Secret is deleted or the key is revoked, tender creates a new key, then deletes every other key of that service account. To rotate, delete the Secret.
- **Deletion:** the service account (and its keys) is always deleted. The Secret is owned by the Bucket and garbage-collected. What happens to the bucket depends on `deletionPolicy`.

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
  - https://github.com/paguos/tender-operator//config/default?ref=v1.0.0
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
