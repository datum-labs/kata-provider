# Ships the Kata runtime-rs shim as a container image so the node installer has
# nothing to download at boot. The upstream release tarball is 1.5 GB and only
# the shim is wanted, so the fetch happens once here at build time.

FROM alpine:3.22 AS artifacts

# Pin the release and its checksum together. The tarball is served from a
# mutable release page, so the digest is what makes the build reproducible.
ARG KATA_VERSION=3.32.0
ARG KATA_TARBALL_SHA256=1449ecea50bd91fa73a94648db195d18950fe869ba4b1f12d05f55f1fa7c1b01
# The shim as published by the Kata project. Checking it after extraction fails
# the build if the tarball layout changes.
ARG KATA_SHIM_SHA256=d7dae95f54cce158edc42e2de167712ab4dee3b64858a53a4e084cf851f5cb19

RUN apk add --no-cache curl tar zstd

RUN set -eu; \
    tarball="kata-static-${KATA_VERSION}-amd64.tar.zst"; \
    curl -fsSL --retry 3 -o "/tmp/${tarball}" \
      "https://github.com/kata-containers/kata-containers/releases/download/${KATA_VERSION}/${tarball}"; \
    echo "${KATA_TARBALL_SHA256}  /tmp/${tarball}" | sha256sum -c; \
    zstd -dc "/tmp/${tarball}" \
      | tar -x -C /tmp ./opt/kata/runtime-rs/bin/containerd-shim-kata-v2; \
    mkdir -p /artifacts; \
    # Renamed on the way in. Both the Go and the Rust runtime build a binary
    # called containerd-shim-kata-v2, and the installer writes into a directory
    # containerd searches first, so the upstream name would replace the Go
    # shim.
    install -m 0755 /tmp/opt/kata/runtime-rs/bin/containerd-shim-kata-v2 \
      /artifacts/containerd-shim-katars-v2; \
    echo "${KATA_SHIM_SHA256}  /artifacts/containerd-shim-katars-v2" | sha256sum -c; \
    cd /artifacts; \
    # The installer re-checks this after copying onto the node, so a truncated
    # placement fails.
    sha256sum containerd-shim-katars-v2 > containerd-shim-katars-v2.sha256; \
    rm -rf /tmp/opt "/tmp/${tarball}"

# The installer runs its copy script in this image, so it needs a shell.
FROM alpine:3.22
COPY --from=artifacts /artifacts /artifacts
