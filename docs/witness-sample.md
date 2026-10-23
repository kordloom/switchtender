<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-train-dark.png">
    <img src="../assets/logo-train.png" alt="SwitchTender" width="140">
  </picture>
</p>

# Witnessed sample

This is a real export from an install enrolled with the [hosted witness](witness.md), published with
the witness's signed statement for that install. Download both and run `switchtender verify
--witness` to see what the check holds an export to, and edit either file to watch it fail.

## What was captured

On October 22, 2026, a fresh install of the signed v1.106.1 Linux build was initialized in a
container under an account named `operator`. It loaded the starter templates, enrolled with the
hosted witness at `https://witness.switchtender.com`, ran three jobs, and pushed a span beat every 30
seconds. Once the witness had countersigned six beats, the install exported its bundle and fetched
the witness's statement. Neither file was changed after that.

The install was a test, so its record was then retired at the witness, and the live statement route
no longer answers for it. The statement here is the witness's signed copy, and it verifies offline
against the published witness key.

| File | What |
|------|------|
| [witness-sample-bundle.json](https://switchtender.com/verify/witness-sample-bundle.json) | The install's signed audit bundle, 20 chain entries. |
| [witness-sample-statement.json](https://switchtender.com/verify/witness-sample-statement.json) | The witness's signed statement for the install's producer key. |

The two keys to pin:

- Producer key, the install that signed the bundle:
  `sha256:cf6f2cc12770ba430de35b1db479178c1fd57c5894d988ae907ec12eea97c6c8`
- Witness key, the one [the hosted witness page](witness.md#the-witness-key) publishes:
  `sha256:82047e9e6247134152a5ca482c61c1f642899586c6bc5562f1dbe3e110ca2804`

## Verify it

    curl -fsSO https://switchtender.com/verify/witness-sample-bundle.json
    curl -fsSO https://switchtender.com/verify/witness-sample-statement.json
    switchtender verify witness-sample-bundle.json \
      --pubkey sha256:cf6f2cc12770ba430de35b1db479178c1fd57c5894d988ae907ec12eea97c6c8 \
      --witness witness-sample-statement.json \
      --witness-pubkey sha256:82047e9e6247134152a5ca482c61c1f642899586c6bc5562f1dbe3e110ca2804

It prints:

```
subject      fleet in_cf6f2cc12770ba430de35b1db479178c
signed by    sha256:cf6f2cc12770ba430de35b1db479178c1fd57c5894d988ae907ec12eea97c6c8
signature    OK
pin          OK (matches the fingerprint you pinned, so this is that install's key)
chain        OK (20 entries recompute, head seq 20)
anchors      none (nothing outside this install fixes its position)
witness      OK (6 beats countersigned by sha256:82047e9e6247134152a5ca482c61c1f642899586c6bc5562f1dbe3e110ca2804, newest seq 20, for producer sha256:cf6f2cc12770ba430de35b1db479178c1fd57c5894d988ae907ec12eea97c6c8, 1 note)
  this bundle reproduces seq 20, the newest entry the witness countersigned. The tree over its first 20 entries matches the root the witness holds. The witness refused no push.
disclosed    24 checked, 0 unchecked, 0 redacted
  checked    beat 6, cadence_s 6, count 6, stream 6

VERIFIED: nothing has been altered since this bundle was signed
```

## How to read it

- **signature** and **pin** say the bundle was signed by the producer key you pinned, so it came from
  that install.
- **chain** recomputes every entry's link from its content. An entry edited after it was written
  breaks the link at that entry.
- **anchors** reads none because this install never anchored to a public timestamp authority. The
  witness is the only party outside the install that holds its position here.
- **witness** holds the bundle to the statement. The statement verifies under the pinned witness
  key and names the same producer key and install. The bundle carries entry 20, the newest one the
  witness countersigned, and the tree over its 20 entries has the root the witness holds. The
  witness refused no push from this install. The one note is the witness's record of the first
  tree it saw, which is where its memory of the install starts.
- **disclosed** lists the beat fields verify checked against the chain.

## Break it

Edit the statement so it claims a refusal, then verify again:

    jq '.refusals_total = 1' witness-sample-statement.json > edited.json
    switchtender verify witness-sample-bundle.json \
      --pubkey sha256:cf6f2cc12770ba430de35b1db479178c1fd57c5894d988ae907ec12eea97c6c8 \
      --witness edited.json \
      --witness-pubkey sha256:82047e9e6247134152a5ca482c61c1f642899586c6bc5562f1dbe3e110ca2804

The witness signed the statement, so any edit to it fails under the witness key:

```
NOT VERIFIED: the witness statement does not hold: the witness statement does not verify: verify statement: signature: does not verify under key sha256:82047e9e6247134152a5ca482c61c1f642899586c6bc5562f1dbe3e110ca2804
```

## What it does not show

- The witness fixes the chain from the first tree it countersigned onward. It shows the entries
  have not changed since that beat. It cannot show they were honest when they were written.
- Between two beats the witness sees nothing. This install beat every 30 seconds, and the cadence
  is the install's to choose.
- Root on the witness host, or a holder of the witness key's backup and its passphrase, can sign
  false countersignatures. [The hosted witness page](witness.md#limits) lists the rest of its limits.
- This is a test install with three short jobs, not a production fleet.
