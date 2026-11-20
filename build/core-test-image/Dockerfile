ARG GO_IMAGE=golang:1.26.11@sha256:7af425da11f4fc1a9308eb704369e3f6d1c44e35bac0f874a59ade3f20fdc07e
FROM ${GO_IMAGE}
ARG DEBIAN_MIRROR=deb.debian.org

# core-test runs on linux/amd64. Match the ShellCheck version on the GitHub runner.
RUN set -eu; \
    curl -fsSL --retry 2 --connect-timeout 8 --max-time 45 \
        "https://${DEBIAN_MIRROR}/debian/pool/main/s/shellcheck/shellcheck_0.9.0-1_amd64.deb" \
        -o /tmp/shellcheck.deb; \
    echo '8c40f8a4ecccf3d746b933853972f1cd1be6df84fc5977d0137269d4debefa4f  /tmp/shellcheck.deb' | sha256sum -c -; \
    dpkg -i /tmp/shellcheck.deb; \
    rm /tmp/shellcheck.deb; \
    shellcheck --version
