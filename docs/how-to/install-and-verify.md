# Install and verify a release

Releases are cut by pushing a `v*` tag; the
[`release` workflow](../../.github/workflows/release.yml) builds, signs, and
publishes the binaries.

Each release ships raw binaries for **linux/amd64**, **linux/arm64**, and
**darwin/arm64**, plus a `checksums.txt`, a cosign signature (`.sig`) and
certificate (`.pem`) for every file, and SPDX and CycloneDX SBOMs.

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
identity. Download `$ASSET.sig` and `$ASSET.pem` alongside the binary:

```sh
cosign verify-blob \
  --certificate $ASSET.pem \
  --signature $ASSET.sig \
  --certificate-identity-regexp 'https://github.com/B42Labs/dizzy/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  $ASSET
```

A successful verification prints `Verified OK`. It proves the binary was built by
this repository's release workflow, not merely that it hashes correctly.

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

Run `dizzy` from anywhere with API access: an operator workstation or a manager
node.
