# Ships the Kata runtime-rs shim as a container image so the node installer has
# nothing to download at boot. The upstream release tarball is 1.5 GB and only
# the shim and the files it launches are wanted, so the fetch happens once here
# at build time.
#
# The shim is shipped with the Cloud Hypervisor binary, guest kernel and guest
# image from the same Kata release. The Talos kata-containers extension carries
# its own copies of these, but they follow the Talos release rather than this
# one. A Cloud Hypervisor newer than the shim rejects the shim's VM
# configuration, so taking them from the node ties every sandbox start to the
# Talos version.

FROM alpine:3.22 AS artifacts

# Pin the release and its checksum together. The tarball is served from a
# mutable release page, so the digest is what makes the build reproducible.
ARG KATA_VERSION=3.32.0
ARG KATA_TARBALL_SHA256=1449ecea50bd91fa73a94648db195d18950fe869ba4b1f12d05f55f1fa7c1b01
# Each file as published by the Kata project. Checking them after extraction
# fails the build if the tarball layout changes. The kernel and image are the
# files the release's vmlinux.container and kata-containers.img links point at.
ARG KATA_SHIM_SHA256=d7dae95f54cce158edc42e2de167712ab4dee3b64858a53a4e084cf851f5cb19
ARG KATA_CLH_SHA256=942917e611cdde4209a155a4b39340d2a24bcf5684190569d179a521fe779569
ARG KATA_KERNEL=vmlinux-6.18.35-197
ARG KATA_KERNEL_SHA256=43701715ae2885f936bbe5c66a2de7c14dc51de7d19412d04833e4bbcf205bd0
ARG KATA_IMAGE=kata-ubuntu-noble.image
ARG KATA_IMAGE_SHA256=e9548ff64f51c120791d3a2d1a81ebfd275df2bf0737368bd3e6381a6e967855

RUN apk add --no-cache curl tar zstd

RUN set -eu; \
    tarball="kata-static-${KATA_VERSION}-amd64.tar.zst"; \
    curl -fsSL --retry 3 -o "/tmp/${tarball}" \
      "https://github.com/kata-containers/kata-containers/releases/download/${KATA_VERSION}/${tarball}"; \
    echo "${KATA_TARBALL_SHA256}  /tmp/${tarball}" | sha256sum -c; \
    zstd -dc "/tmp/${tarball}" \
      | tar -x -C /tmp \
          ./opt/kata/runtime-rs/bin/containerd-shim-kata-v2 \
          ./opt/kata/bin/cloud-hypervisor \
          "./opt/kata/share/kata-containers/${KATA_KERNEL}" \
          "./opt/kata/share/kata-containers/${KATA_IMAGE}"; \
    mkdir -p /artifacts/runtime-rs/bin /artifacts/runtime-rs/share; \
    # Renamed on the way in. Both the Go and the Rust runtime build a binary
    # called containerd-shim-kata-v2, and the installer writes into a directory
    # containerd searches first, so the upstream name would replace the Go
    # shim.
    install -m 0755 /tmp/opt/kata/runtime-rs/bin/containerd-shim-kata-v2 \
      /artifacts/containerd-shim-katars-v2; \
    install -m 0755 /tmp/opt/kata/bin/cloud-hypervisor \
      /artifacts/runtime-rs/bin/cloud-hypervisor; \
    install -m 0644 "/tmp/opt/kata/share/kata-containers/${KATA_KERNEL}" \
      /artifacts/runtime-rs/share/vmlinux.container; \
    install -m 0644 "/tmp/opt/kata/share/kata-containers/${KATA_IMAGE}" \
      /artifacts/runtime-rs/share/kata-containers.img; \
    cd /artifacts; \
    { \
      echo "${KATA_SHIM_SHA256}  containerd-shim-katars-v2"; \
      echo "${KATA_CLH_SHA256}  runtime-rs/bin/cloud-hypervisor"; \
      echo "${KATA_KERNEL_SHA256}  runtime-rs/share/vmlinux.container"; \
      echo "${KATA_IMAGE_SHA256}  runtime-rs/share/kata-containers.img"; \
    } > SHA256SUMS; \
    # The installer re-checks these after copying onto the node, so a truncated
    # placement fails.
    sha256sum -c SHA256SUMS; \
    rm -rf /tmp/opt "/tmp/${tarball}"

# The installer runs its copy script in this image, so it needs a shell.
FROM alpine:3.22
COPY --from=artifacts /artifacts /artifacts
