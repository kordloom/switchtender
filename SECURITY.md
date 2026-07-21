# Security Policy

## Reporting a vulnerability

Report suspected vulnerabilities privately to security@switchtender.com. Do not open a public issue
for a security problem.

Include as much as you can:

- A description of the issue and its impact.
- Steps to reproduce, or a proof of concept.
- The affected version, commit, or configuration.

You will get an acknowledgment within a few business days. Once a fix is ready, a patched release
goes out, and the report is credited unless you prefer to stay anonymous.

## Supported versions

Security fixes land on the latest 1.x release. Older versions are not patched.

## Scope

The server, the CLI, the SDK, and the official container image and Helm chart are in scope. Report
issues in third-party dependencies upstream, though a heads-up here is welcome.

## Verifying a release

Every release ships a `SHA256SUMS` file signed with [cosign](https://docs.sigstore.dev) using
keyless signing tied to this repository's GitHub Actions identity. There is no long-lived key to
steal, and the signature is recorded in the public Rekor transparency log.

Verify the signature over the checksums, then the archive against them:

    cosign verify-blob SHA256SUMS \
      --signature SHA256SUMS.sig \
      --certificate SHA256SUMS.pem \
      --certificate-identity-regexp '^https://github.com/kordloom/switchtender/.github/workflows/release.yml@.*' \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com

    shasum -a 256 -c SHA256SUMS --ignore-missing

## Security posture

- Secrets are encrypted at rest with AES-GCM under an Argon2id-derived key, decrypt only inside the
  executing process, never serialize into API responses, and are masked out of run logs.
- Every run's changes are linked into a tamper-evident SHA-256 hash chain that verifies offline
  without trusting the server.
- Container execution environments run under memory, CPU, process, and network caps, refuse to mount
  sensitive host paths or the container socket, and are off by default.
- Every change is scanned with govulncheck and CodeQL, the fuzz corpus runs on a schedule, and
  dependency updates are tracked by Dependabot. Each release ships an SPDX software bill of materials.
