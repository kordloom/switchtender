# Embedded witness

A complete third-party witness in one file, built from the public packages alone: `identity` for
the signing key, `witness` for the memory and the findings, and through it `beatfeed` for the wire
contract. It imports nothing from `internal/`, so the same code compiles in your own module.

A chain proves what it holds was not altered. It cannot prove nothing was removed from the end,
because the process that runs the chain also decides what gets written down. This program is the
outside party that closes that: it remembers what the feed served in a checkpoint signed with its
own key, and reports a missing beat, a rewritten beat, or a head that went backward.

```
go run ./examples/embedded-witness --server https://st.example.com
```

Run it where the watched server's operator has no hand. The built-in `switchtender witness` command
is this same loop with a webhook and a cron mode; `switchtender witness serve` is the hosted,
many-server form that answers auditors with countersigned attestations.

The other direction embeds too: the `spanbeat` package emits the beats this witness watches, and it
couples to a chain only through its small `Store` interface, so a service with its own audit chain
can give it an attestable heartbeat the same way.
