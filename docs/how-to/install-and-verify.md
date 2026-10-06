# Install and verify a release

Releases are cut by pushing a `v*` tag; the
[`release` workflow](../../.github/workflows/release.yml) builds, signs, and
publishes the binaries.

Each release ships raw binaries for **linux/amd64**, **linux/arm64**, and
**darwin/arm64**, plus a `checksums.txt`, a cosign signature bundle (`.bundle`)
for every file, and SPDX and CycloneDX SBOMs. Each release after `v0.1.0` also
publishes the container image `ghcr.io/b42labs/dizzy:<tag>` for **linux/amd64**
and **linux/arm64**, signed with cosign and carrying an SPDX SBOM attestation
per platform.

## Install a binary

```sh
VERSION=v0.1.0
ASSET=dizzy-linux-amd64

curl -fsSLO https://github.com/B42Labs/dizzy/releases/download/$VERSION/$ASSET
curl -fsSLO https://github.com/B42Labs/dizzy/releases/download/$VERSION/checksums.txt

grep " $ASSET$" checksums.txt | sha256sum -c -
chmod +x $ASSET
sudo install $ASSET /usr/local/bin/dizzy

dizzy --version
```

On macOS, `sha256sum` is `shasum -a 256 -c -`.

## Verify the cosign signature

The binaries are signed keyless via Sigstore, with GitHub Actions as the OIDC
identity. Download `$ASSET.bundle` alongside the binary:

```sh
cosign verify-blob \
  --bundle $ASSET.bundle \
  --certificate-identity-regexp 'https://github.com/B42Labs/dizzy/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  $ASSET
```

A successful verification prints `Verified OK`. It proves the binary was built by
this repository's release workflow, not merely that it hashes correctly.

## Use the container image

The image is `ghcr.io/b42labs/dizzy:<tag>`. The rows below are stable across
releases; the base image is not part of the contract.

| What | Value |
|---|---|
| Image | `ghcr.io/b42labs/dizzy` (lowercase; a registry rejects `B42Labs`) |
| Tag | the git tag verbatim, with its `v`, for example `v0.2.0`. No other tag |
| Platforms | `linux/amd64`, `linux/arm64` |
| Binary | `/usr/local/bin/dizzy`, mode 0755, statically linked, identical to the release asset `dizzy-linux-<arch>` |
| Profiles | `/usr/share/dizzy/scenarios/<service>/<profile>.yaml`, the YAML files of `scenarios/` in the tagged commit |
| Shell | `/bin/sh` and `cp` on `PATH` |
| Entrypoint | `["/usr/local/bin/dizzy"]`, no `CMD` |
| User | `65534:65534` (numeric, so `runAsNonRoot: true` passes) |
| Working directory | `/work`, owned by `65534:65534`. dizzy writes `run-<id>.json` here |
| CA bundle | `/etc/ssl/certs/ca-certificates.crt` |

### Run dizzy from the image

```sh
VERSION=v0.2.0

docker run --rm \
  --user "$(id -u):$(id -g)" \
  --mount "type=bind,src=$HOME/.config/openstack/clouds.yaml,dst=/etc/openstack/clouds.yaml,readonly" \
  -v "$PWD:/work" \
  -e OS_CLOUD=mycloud \
  ghcr.io/b42labs/dizzy:$VERSION \
  neutron apply --scenario /usr/share/dizzy/scenarios/neutron/small.yaml
```

The run record lands in the mounted `/work`. `--user` makes that directory
writable for the caller; in Kubernetes, a pod that mounts a volume at `/work`
sets `fsGroup` or `runAsUser` to match the volume. `--mount` stops with an error
when `clouds.yaml` is not at that path, where `-v` would create a root-owned
directory in its place.

### Copy the binary into another container

```yaml
initContainers:
  - name: dizzy
    image: ghcr.io/b42labs/dizzy:v0.2.0@sha256:<index-digest>
    command: ["cp", "/usr/local/bin/dizzy", "/tools/dizzy"]
    securityContext: &restricted
      runAsNonRoot: true
      runAsUser: 65534
      runAsGroup: 65534
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: ["ALL"]
      seccompProfile:
        type: RuntimeDefault
    volumeMounts:
      - name: tools
        mountPath: /tools
containers:
  - name: runner
    image: docker.io/library/alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
    command: ["/tools/dizzy", "--version"]
    securityContext: *restricted
    volumeMounts:
      - name: tools
        mountPath: /tools
volumes:
  - name: tools
    emptyDir: {}
```

The binary is statically linked, so the second container needs nothing from the
dizzy image. Replace `sha256:<index-digest>` with the `$INDEX` you verified in
[Verify the image](#verify-the-image), so the pod runs exactly that image.

### Verify the image

The commands need cosign v3 or newer, and `$VERSION` set as in the run example.
`INDEX` holds the digest the tag resolves to, so every later command checks the
same image even if the tag is moved meanwhile. `IDENTITY` accepts only the
release workflow run for `$VERSION`; a signature made by another workflow or for
another release does not verify. The SBOM attestation is attached per platform
digest, so the last command checks one platform's digest.

```sh
INDEX=$(docker buildx imagetools inspect ghcr.io/b42labs/dizzy:$VERSION \
  --format '{{json .Manifest}}' | jq -r .digest)
IDENTITY=https://github.com/B42Labs/dizzy/.github/workflows/release.yml@refs/tags/$VERSION

cosign verify \
  --certificate-identity "$IDENTITY" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/b42labs/dizzy@$INDEX

DIGEST=$(docker buildx imagetools inspect ghcr.io/b42labs/dizzy@$INDEX --raw \
  | jq -r '.manifests[] | select(.platform.architecture == "amd64") | .digest')

cosign verify-attestation --type spdxjson \
  --certificate-identity "$IDENTITY" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/b42labs/dizzy@$DIGEST
```

Then run the image you verified as `ghcr.io/b42labs/dizzy@$INDEX`, not as
`ghcr.io/b42labs/dizzy:$VERSION`.

## Build from source

Requires Go 1.26 (see `go.mod`).

```console
$ git clone https://github.com/B42Labs/dizzy.git
$ cd dizzy
$ make build          # produces ./dizzy
$ ./dizzy --version   # prints "dizzy dev" for a local build
```

`make install` puts it on your `GOBIN` instead.

The version string comes from a build-time variable, so a local `go build` with
no ldflags reports `dev`.

## Get the scenario profiles

`--scenario` takes a **filesystem path**. The fifteen built-in profiles are not
resolvable by name from an installed binary — they live under `scenarios/` in the
repository. Clone it even if you installed a release binary:

```console
$ git clone https://github.com/B42Labs/dizzy.git
$ dizzy neutron apply --scenario dizzy/scenarios/neutron/small.yaml --dry-run
```

The container image carries the same files under `/usr/share/dizzy/scenarios`.

Or write your own; the format is in the
[scenario schema reference](../reference/scenario-schema.md).

## Configure authentication

`dizzy` reads `clouds.yaml` natively through gophercloud, honoring `$OS_CLOUD`
and the standard search paths: the current directory, `~/.config/openstack`, and
`/etc/openstack`. `OS_CLIENT_CONFIG_FILE` points it at a specific file.

```console
$ export OS_CLOUD=mycloud
$ dizzy neutron list-networks
```

`list-networks` is a read-only smoke test. If it lists your project's networks,
authentication and connectivity are working.

If your cloud uses a private CA, the certificate must be trusted — either
system-wide, or through the `cacert` key in the `clouds.yaml` entry.

Run `dizzy` from anywhere with API access: an operator workstation, a manager
node, or a pod in a cluster.
