# Releasing

Releases are automatic. Every merge to `main` that contains a `feat:` or `fix:` commit becomes a new version.

## The flow

On every push to `main`, `.github/workflows/ci.yml` runs:

1. **lint** and **test**. A red `main` is never released.
2. **release**: [release-please](https://github.com/googleapis/release-please) opens a `chore(main): release X.Y.Z` PR, the job merges it straight away, and a second release-please pass tags `vX.Y.Z` and creates the GitHub Release. The release PR also bumps the version in `config/manager/kustomization.yaml` (the image tag), `config/default/kustomization.yaml` and `README.md`, so `config/default?ref=vX.Y.Z` installs image `X.Y.Z`.
3. **publish** (releases only): builds `ghcr.io/paguos/tender-operator` for linux/amd64 and linux/arm64, tagged `X.Y.Z`, the short SHA and `latest`.
4. **deploy** (optional): runs `scripts/deploy-image.sh <app> <env> X.Y.Z` in the repository named by the `DEPLOY_REPO` variable. Skipped when that variable is unset.

## Versions

- [Conventional Commits](https://www.conventionalcommits.org/). With squash merges, the **PR title** is the commit, so PR titles must be conventional.
- `fix:` gives a patch release and `feat:` a minor one. Before 1.0, `feat!:` also only bumps the minor. `chore:`, `ci:`, `docs:`, `refactor:` and `test:` don't release.
- Force a version with a `Release-As: 1.0.0` footer.

## Rollback

Released images are never overwritten. Pin an older `?ref=vX.Y.Z` (or image tag) wherever tender is installed. CRD changes don't roll back on their own: check `config/crd/` between the two versions first.

## One-time setup

- Settings → Actions → General: "Allow GitHub Actions to create and approve pull requests" (release-please needs it).
- Branch protection on `main` must not require reviews or checks, because the bot merges its own release PR, and checks don't run on PRs opened by `GITHUB_TOKEN`.
- After the first publish, make the package public (Package settings → Change visibility) so clusters can pull without credentials.
- Optional deploys: set the variables `DEPLOY_REPO` (`owner/repo`), `DEPLOY_APP` and `DEPLOY_ENV`, and the secret `DEPLOY_TOKEN` (Contents read/write on that repo only).
