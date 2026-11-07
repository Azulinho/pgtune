# Package repositories

These scripts turn the `.deb` and `.rpm` packages attached to a GitHub release
into signed package repositories, which are then served by GitHub Pages.

They are run by [`.github/workflows/pages.yml`](../../.github/workflows/pages.yml)
after `release.yml` publishes a release, and can also be run locally:

```bash
# Build both repositories from the packages of one release.
PKG_DIR=dist OUT_DIR=_site/deb GPG_KEY_ID=<key id> \
  packaging/repo/build-deb-repo.sh
PKG_DIR=dist OUT_DIR=_site/rpm GPG_KEY_ID=<key id> \
  packaging/repo/build-rpm-repo.sh
```

Set `GPG_PASSPHRASE_FILE` when the signing key is passphrase-protected.

## Published layout

```
deb/
  dists/stable/InRelease, Release, Release.gpg
  dists/stable/main/binary-{amd64,arm64}/Packages(.gz)
  pool/main/p/pgtune/*.deb
  pgtune-archive-keyring.gpg
rpm/
  repodata/repomd.xml, repomd.xml.asc
  *.rpm
  RPM-GPG-KEY-pgtune.asc
  pgtune.repo
```

Only the release passed in `PKG_DIR` is published; the repository is rebuilt
from scratch on every run and older packages remain on the Releases page.
Dispatching `pages.yml` for an older tag therefore rolls the repositories back
to that release, so normally only the newest tag should be deployed.

## One-time setup

1. Create a signing key:

   ```bash
   gpg --quick-gen-key "pgtune archive <git@azulinho.com>" rsa4096 sign never
   ```

2. Add the ASCII-armored private key as the `GPG_PRIVATE_KEY` Actions secret,
   and its passphrase (if any) as `GPG_PASSPHRASE`:

   ```bash
   gpg --armor --export-secret-keys <key id> | gh secret set GPG_PRIVATE_KEY
   ```

   If the key has no passphrase, leave the `GPG_PASSPHRASE` secret unset.

3. Set the Pages source to **GitHub Actions** in the repository settings.

4. If the site uses a custom domain, set the `REPO_BASE_URL` environment
   variable in `.github/workflows/pages.yml` and add a `CNAME` file to
   `_site/`.

The public key is exported into the repository on every build, so it always
matches the key used to sign the metadata.
