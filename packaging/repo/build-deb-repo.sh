#!/usr/bin/env bash
#
# Build a signed APT repository from a set of .deb packages.
#
# The repository uses the standard Debian layout (dists/ + pool/) so it can be
# served by any static web host, including GitHub Pages:
#
#   <out>/dists/stable/InRelease
#   <out>/dists/stable/Release
#   <out>/dists/stable/Release.gpg
#   <out>/dists/stable/main/binary-<arch>/Packages(.gz)
#   <out>/pool/main/p/pgtune/*.deb
#   <out>/pgtune-archive-keyring.gpg
#
# The repository is rebuilt from scratch on every run: only the packages passed
# in PKG_DIR are published.
#
# Usage:
#   PKG_DIR=dist OUT_DIR=_site/deb GPG_KEY_ID=<key id> \
#     packaging/repo/build-deb-repo.sh
#
# Required environment:
#   PKG_DIR    directory containing the .deb files
#   OUT_DIR    destination directory for the generated repository
#   GPG_KEY_ID key ID or fingerprint of the signing key in the local keyring
#
# Optional environment:
#   ARCHES              Debian architectures to publish (default: amd64 arm64)
#   GPG_PASSPHRASE_FILE file containing the signing key passphrase, if any
#
set -euo pipefail

: "${PKG_DIR:?PKG_DIR is required}"
: "${OUT_DIR:?OUT_DIR is required}"
: "${GPG_KEY_ID:?GPG_KEY_ID is required}"

ARCHES=${ARCHES:-"amd64 arm64"}
DIST=${DIST:-stable}
COMPONENT=${COMPONENT:-main}

for tool in reprepro gpg; do
  command -v "${tool}" >/dev/null 2>&1 ||
    { echo "error: ${tool} is required" >&2; exit 1; }
done

if [ ! -d "${PKG_DIR}" ]; then
  echo "error: package directory ${PKG_DIR} does not exist" >&2
  exit 1
fi

gpg_args=(--batch --yes --local-user "${GPG_KEY_ID}")
if [ -n "${GPG_PASSPHRASE_FILE:-}" ]; then
  gpg_args+=(--pinentry-mode loopback --passphrase-file "${GPG_PASSPHRASE_FILE}")
fi

# Fail early if a requested architecture is missing instead of publishing a
# repository that silently cannot serve it.
for arch in ${ARCHES}; do
  if ! compgen -G "${PKG_DIR}/*_${arch}.deb" >/dev/null; then
    echo "error: no .deb for architecture ${arch} in ${PKG_DIR}" >&2
    exit 1
  fi
done

work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT

mkdir -p "${work}/repo/conf"
cat >"${work}/repo/conf/distributions" <<EOF
Origin: pgtune
Label: pgtune
Suite: ${DIST}
Codename: ${DIST}
Architectures: ${ARCHES}
Components: ${COMPONENT}
Description: pgtune APT repository
EOF

# --export=never keeps reprepro from exporting (and trying to sign) after
# every package; do it once at the end instead.
reprepro --basedir "${work}/repo" --export=never includedeb "${DIST}" \
  "${PKG_DIR}"/*.deb
reprepro --basedir "${work}/repo" export "${DIST}"

rm -rf "${OUT_DIR}/dists" "${OUT_DIR}/pool"
mkdir -p "${OUT_DIR}"
cp -a "${work}/repo/dists" "${OUT_DIR}/"
cp -a "${work}/repo/pool" "${OUT_DIR}/"

# Sign the repository metadata: InRelease is a clearsigned Release, Release.gpg
# is kept for older clients that predate InRelease.
gpg "${gpg_args[@]}" --clearsign \
  --output "${OUT_DIR}/dists/${DIST}/InRelease" \
  "${OUT_DIR}/dists/${DIST}/Release"
gpg "${gpg_args[@]}" --detach-sign --armor \
  --output "${OUT_DIR}/dists/${DIST}/Release.gpg" \
  "${OUT_DIR}/dists/${DIST}/Release"

# Publish the signing key so users can install it as a keyring for
# "deb [signed-by=...]".
gpg --batch --export "${GPG_KEY_ID}" >"${OUT_DIR}/pgtune-archive-keyring.gpg"
gpg --batch --armor --export "${GPG_KEY_ID}" >"${OUT_DIR}/pgtune-archive-keyring.asc"

gpg --batch --verify "${OUT_DIR}/dists/${DIST}/Release.gpg" \
  "${OUT_DIR}/dists/${DIST}/Release"
gpg --batch --verify "${OUT_DIR}/dists/${DIST}/InRelease"

echo "APT repository written to ${OUT_DIR}"
