# The Linux machine scripts/ci-local.sh runs the GitHub workflows on.
#
# CI runs on GitHub's ubuntu-latest runner, and a macOS run of the suite kept passing while CI
# failed: on a Linux-only file the linter only reads for GOOS=linux, on ext4 handing a deleted
# directory's inode to the next directory made, on the runner's own ansible-core and botocore, and
# on jobs nobody ran locally at all. This image is that runner as far as those failures reach: the
# same Ubuntu release on an ext4-backed filesystem, a non-root runner account with passwordless
# sudo, the runner's own pipx ansible-core and an older botocore, and every tool the workflows
# install, at the versions they pin.
#
# Do not build it by hand. scripts/ci-local.sh reads each pinned version out of go.mod and the
# workflows, passes them in as build arguments, and rebuilds the image when one changes, so a pin
# bumped in ci.yml reaches this image without anyone editing this file. The arguments without a
# default have no source but the workflows, and the build refuses to run without them.
FROM ubuntu:24.04

ARG TARGETARCH

# The runner's base packages that the suite reaches: a compiler for cgo and the race detector, git,
# Python 3.12 with venv, openssh-server for the tests that start a local sshd, and socat for the
# Docker socket the runner account is given. python3-cryptography and python3-botocore are the
# system Python's own, older than PyPI's, the way the runner's are: a probe that assumes a newer
# botocore than the runner carries failed in CI, and it fails here for the same reason.
RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      build-essential ca-certificates curl file git gnupg iproute2 jq less libicu74 locales \
      openssh-client openssh-server pipx pkg-config procps python3 python3-botocore \
      python3-cryptography python3-pip python3-venv python3-yaml rsync socat sudo tzdata unzip \
      xz-utils zip; \
    rm -rf /var/lib/apt/lists/*; \
    mkdir -p /run/sshd

# The hosted runner's account: uid 1001, primary group docker (gid 118), passwordless sudo. Tests
# that expect a permission to be refused only see the refusal as a non-root user, and CI's install
# steps call sudo.
RUN set -eux; \
    groupadd --gid 118 docker; \
    useradd --uid 1001 --gid docker --groups adm --create-home --shell /bin/bash runner; \
    printf 'runner ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/runner; \
    chmod 0440 /etc/sudoers.d/runner; \
    mkdir -p /home/runner/go/pkg/mod /home/runner/.cache/go-build /home/runner/work; \
    chown -R runner:docker /home/runner

# Workspaces are bind mounts and volumes owned by another uid, and git refuses to read a repository
# its user does not own unless told it is safe. The hosted runner owns its checkout, so this only
# restores what CI already has.
RUN printf '[safe]\n\tdirectory = *\n' > /etc/gitconfig

# The runner's PATH order, with the directory pipx installs the runner's own tools into ahead of the
# system's. ansible-core lives there on a hosted runner, and a pinned install that does not land
# ahead of it silently tests the runner's version instead.
ENV PATH=/home/runner/.local/bin:/opt/pipx_bin:/usr/local/go/bin:/home/runner/go/bin:$PATH \
    PIPX_HOME=/opt/pipx \
    PIPX_BIN_DIR=/opt/pipx_bin

# Go at the version go.mod names, which is what setup-go's go-version-file installs. GOTOOLCHAIN is
# pinned to local so a stale image fails loudly instead of quietly downloading another toolchain.
ARG GO_VERSION
RUN set -eux; \
    test -n "$GO_VERSION"; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" | tar -xz -C /usr/local; \
    /usr/local/go/bin/go version
ENV GOTOOLCHAIN=local

# Node at the newest release of the major line setup-node is given, checked against its published
# checksum.
ARG NODE_MAJOR
RUN set -eux; \
    test -n "$NODE_MAJOR"; \
    case "$TARGETARCH" in amd64) arch=x64 ;; arm64) arch=arm64 ;; *) exit 1 ;; esac; \
    base="https://nodejs.org/dist/latest-v${NODE_MAJOR}.x"; \
    curl -fsSL -o /tmp/SHASUMS256.txt "$base/SHASUMS256.txt"; \
    file="$(awk -v want="linux-${arch}.tar.xz" '$2 ~ want"$" {print $2}' /tmp/SHASUMS256.txt)"; \
    test -n "$file"; \
    curl -fsSL -o "/tmp/$file" "$base/$file"; \
    (cd /tmp && grep " $file\$" SHASUMS256.txt | sha256sum -c -); \
    tar -xJf "/tmp/$file" -C /usr/local --strip-components=1 --exclude='*.md' --exclude=LICENSE; \
    rm -f "/tmp/$file" /tmp/SHASUMS256.txt; \
    node --version

# The engines the full suite requires. ci.yml installs Terraform and OpenTofu only when the runner
# lacks them, at the versions its env blocks pin, so they are here at those versions.
ARG TERRAFORM_VERSION
ARG TOFU_VERSION
RUN set -eux; \
    test -n "$TERRAFORM_VERSION"; \
    test -n "$TOFU_VERSION"; \
    curl -fsSL -o /tmp/terraform.zip \
      "https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_linux_${TARGETARCH}.zip"; \
    unzip -qo /tmp/terraform.zip terraform -d /usr/local/bin; \
    curl -fsSL -o /tmp/tofu.zip \
      "https://github.com/opentofu/opentofu/releases/download/v${TOFU_VERSION}/tofu_${TOFU_VERSION}_linux_${TARGETARCH}.zip"; \
    unzip -qo /tmp/tofu.zip tofu -d /usr/local/bin; \
    rm -f /tmp/terraform.zip /tmp/tofu.zip; \
    terraform version; \
    tofu version

# What the hosted runner ships rather than what a workflow installs, so no workflow names a version:
# PowerShell, the AWS CLI, kubectl for the supertest, and the Docker CLI with buildx, which the
# supertest and the integration suite drive the host's daemon with.
ARG PWSH_VERSION=7.6.6
ARG KUBECTL_VERSION=1.31.0
ARG DOCKER_CLI_VERSION=27.5.1
ARG BUILDX_VERSION=0.9.1
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) pwsh_arch=x64; aws_arch=x86_64; docker_arch=x86_64 ;; \
      arm64) pwsh_arch=arm64; aws_arch=aarch64; docker_arch=aarch64 ;; \
      *) exit 1 ;; \
    esac; \
    mkdir -p /opt/microsoft/powershell/7; \
    curl -fsSL "https://github.com/PowerShell/PowerShell/releases/download/v${PWSH_VERSION}/powershell-${PWSH_VERSION}-linux-${pwsh_arch}.tar.gz" \
      | tar -xz -C /opt/microsoft/powershell/7; \
    chmod +x /opt/microsoft/powershell/7/pwsh; \
    ln -s /opt/microsoft/powershell/7/pwsh /usr/bin/pwsh; \
    curl -fsSL -o /tmp/awscliv2.zip "https://awscli.amazonaws.com/awscli-exe-linux-${aws_arch}.zip"; \
    unzip -q /tmp/awscliv2.zip -d /tmp; \
    /tmp/aws/install; \
    rm -rf /tmp/aws /tmp/awscliv2.zip; \
    curl -fsSL -o /usr/local/bin/kubectl \
      "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl"; \
    chmod 0755 /usr/local/bin/kubectl; \
    curl -fsSL "https://download.docker.com/linux/static/stable/${docker_arch}/docker-${DOCKER_CLI_VERSION}.tgz" \
      | tar -xz -C /usr/local/bin --strip-components=1 docker/docker; \
    mkdir -p /usr/local/lib/docker/cli-plugins; \
    curl -fsSL -o /usr/local/lib/docker/cli-plugins/docker-buildx \
      "https://github.com/docker/buildx/releases/download/v${BUILDX_VERSION}/buildx-v${BUILDX_VERSION}.linux-${TARGETARCH}"; \
    chmod 0755 /usr/local/lib/docker/cli-plugins/docker-buildx; \
    pwsh --version; \
    aws --version; \
    kubectl version --client; \
    docker --version

# The release binary golangci-lint-action downloads, at the version the lint job names.
ARG GOLANGCI_LINT_VERSION
RUN set -eux; \
    test -n "$GOLANGCI_LINT_VERSION"; \
    v="${GOLANGCI_LINT_VERSION#v}"; \
    curl -fsSL "https://github.com/golangci/golangci-lint/releases/download/v${v}/golangci-lint-${v}-linux-${TARGETARCH}.tar.gz" \
      | tar -xz -C /tmp; \
    mv "/tmp/golangci-lint-${v}-linux-${TARGETARCH}/golangci-lint" /usr/local/bin/golangci-lint; \
    rm -rf "/tmp/golangci-lint-${v}-linux-${TARGETARCH}"; \
    golangci-lint version

# The files CI's own steps download, kept under the exact URLs those steps name, for the runner's
# curl to serve offline. The steps fetch the linux-amd64 builds by name and check each against its
# pinned SHA-256. On amd64 every file is that upstream download byte for byte, so the pins are
# checked here as in CI. On another architecture each is this machine's build in the same layout:
# the step that unpacks helm still finds linux-amd64/helm, and it runs here.
ARG HELM_VERSION
ARG GITLEAKS_VERSION
ARG KIND_VERSION
RUN set -eux; \
    test -n "$HELM_VERSION"; \
    test -n "$GITLEAKS_VERSION"; \
    test -n "$KIND_VERSION"; \
    case "$TARGETARCH" in \
      amd64) gitleaks_arch=x64 ;; \
      arm64) gitleaks_arch=arm64 ;; \
      *) exit 1 ;; \
    esac; \
    d=/opt/ci-local/downloads; \
    mkdir -p "$d"; \
    helm_url="https://get.helm.sh/helm-v${HELM_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    if [ "$TARGETARCH" = amd64 ]; then \
      curl -fsSL -o "$d/helm.tar.gz" "$helm_url"; \
    else \
      mkdir -p "$d/helm"; \
      curl -fsSL "$helm_url" | tar -xz -C "$d/helm"; \
      mv "$d/helm/linux-${TARGETARCH}" "$d/helm/linux-amd64"; \
      tar -czf "$d/helm.tar.gz" -C "$d/helm" linux-amd64; \
      rm -rf "$d/helm"; \
    fi; \
    gitleaks_url="https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}"; \
    curl -fsSL -o "$d/gitleaks.tar.gz" \
      "$gitleaks_url/gitleaks_${GITLEAKS_VERSION}_linux_${gitleaks_arch}.tar.gz"; \
    curl -fsSL -o "$d/kind" "https://kind.sigs.k8s.io/dl/v${KIND_VERSION}/kind-linux-${TARGETARCH}"; \
    { \
      printf '{\n'; \
      printf '  "https://get.helm.sh/helm-v%s-linux-amd64.tar.gz": "helm.tar.gz",\n' "$HELM_VERSION"; \
      printf '  "%s/gitleaks_%s_linux_x64.tar.gz": "gitleaks.tar.gz",\n' \
        "$gitleaks_url" "$GITLEAKS_VERSION"; \
      printf '  "https://kind.sigs.k8s.io/dl/v%s/kind-linux-amd64": "kind"\n' "$KIND_VERSION"; \
      printf '}\n'; \
    } > "$d/index.json"; \
    python3 -c 'import json, sys; json.load(open(sys.argv[1]))' "$d/index.json"

# Every Python package a workflow pip-installs, at its pin, as wheels CI's own venv steps install
# from offline. Then the runner's own ansible-core, installed with pipx under the root-owned home a
# hosted runner uses, at a release no workflow pins: a job whose pinned install does not end up
# ahead of it on PATH tests this one, and the conformance test's version check fails here as it did
# in CI.
ARG ANSIBLE_CORE_VERSIONS
ARG CRYPTOGRAPHY_VERSION
ARG RUNNER_ANSIBLE_CORE=2.19.0
RUN set -eux; \
    test -n "$ANSIBLE_CORE_VERSIONS"; \
    test -n "$CRYPTOGRAPHY_VERSION"; \
    for v in $ANSIBLE_CORE_VERSIONS; do \
      if [ "$v" = "$RUNNER_ANSIBLE_CORE" ]; then \
        echo "RUNNER_ANSIBLE_CORE must differ from every pinned ansible-core"; \
        exit 1; \
      fi; \
    done; \
    python3 -m venv /tmp/fetch; \
    fetch="/tmp/fetch/bin/pip download --quiet --only-binary=:all: --dest /opt/ci-local/wheels"; \
    for v in $ANSIBLE_CORE_VERSIONS; do \
      $fetch "ansible-core==$v"; \
    done; \
    $fetch "cryptography==$CRYPTOGRAPHY_VERSION"; \
    rm -rf /tmp/fetch; \
    pipx install "ansible-core==$RUNNER_ANSIBLE_CORE"; \
    /opt/pipx_bin/ansible --version

# Playwright's Chromium and its system libraries, for the version internal/ui/e2e locks, and the npm
# cache its npm ci reads. ci-local.sh rebuilds the image when the lockfile changes.
COPY e2e/package.json e2e/package-lock.json /opt/ci-local/e2e/
RUN set -eux; \
    chown -R runner:docker /opt/ci-local/e2e; \
    cd /opt/ci-local/e2e; \
    sudo -u runner -H npm ci --no-audit --no-fund; \
    ./node_modules/.bin/playwright install-deps chromium; \
    sudo -u runner -H ./node_modules/.bin/playwright install chromium; \
    rm -rf /opt/ci-local/e2e/node_modules /var/lib/apt/lists/*

# The wheels the managed Ansible runtime's locks pin, for this image's Python, checked against the
# locks' hashes as they download. The test that installs a real runtime then runs here offline from
# the wheel directory, the way an offline install does, and pip checks the same hashes again.
COPY ansible-locks/ /opt/ci-local/ansible-locks/
RUN set -eux; \
    python3 -m venv /tmp/fetch; \
    for lock in /opt/ci-local/ansible-locks/*.txt; do \
      /tmp/fetch/bin/pip download --quiet --require-hashes --only-binary=:all: \
        --dest /opt/ci-local/wheels -r "$lock"; \
    done; \
    rm -rf /tmp/fetch

# CI's venv steps install from the wheels above without reaching PyPI.
ENV PIP_NO_INDEX=1 \
    PIP_FIND_LINKS=/opt/ci-local/wheels \
    PIP_DISABLE_PIP_VERSION_CHECK=1

WORKDIR /home/runner/work
