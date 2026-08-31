# Continuity

What happens to your install, your evidence, and your access to the source if KordLoom
disappears tomorrow. Vendor risk reviews ask this about every one-person company, so here is the
whole answer in one place.

## Your install keeps running

The binary is self-hosted and self-contained. There is no license server, no online activation,
and no phone-home, so nothing stops working when the vendor does. A lapsed paid subscription
stops paid services from being delivered; it never degrades the software you run.

## Your evidence stays verifiable forever

Every audit bundle and run receipt verifies offline with the open verifier, against the open
LoomSeal format. Verification needs no KordLoom service, no network, and no permission. Evidence
you exported years ago verifies the same way on the day an auditor asks. The witness command
ships in the binary itself, so a countersigning witness can be run by anyone, including you.

## The source outlives the vendor

Three layers, strongest first:

1. Every release attaches its own source tarball as a release asset, listed in the signed
   `SHA256SUMS` manifest beside the binaries. Keeping a release means keeping its exact source.
2. The license converts. Each release becomes Apache 2.0 two years after it ships, automatically,
   with no trigger event, agent, or dispute. The conversion is written into the license itself.
3. You can mirror the repository today, and you should if the software matters to you:

```sh
git clone --mirror https://github.com/kordloom/switchtender.git
cd switchtender.git && git fetch --all --tags   # repeat on a schedule
```

Under BSL 1.1 you may copy, modify, and run the source in production for your own organization
right now. Continuity does not wait for the conversion date.

## What degrades on vendor death

Honesty requires the other column. Vendor-operated services stop: hosted witness
countersignatures, scheduled delivery of paid evidence packs, and support. Each has a
self-service fallback in the free binary (run your own witness, the change-register emitter, the
community), but the operated versions are what a subscription buys, and they end with the vendor.
