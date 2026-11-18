# Third-Party Notices

SwitchTender is distributed under the Business Source License 1.1. The binary this repository builds
incorporates the third-party Go modules listed below, each under its own open-source license. No
dependency is licensed under the GPL, LGPL, or AGPL. Full license texts are available in each
module's source repository.

The lists below are the modules `go version -m` prints for the macOS release binary, which is built
with the Go version `go.mod` names, with each license read by `go-licenses` 1.6.0. Two licenses were
read by hand from the module's own LICENSE file: `modernc.org/mathutil`, which the tool could not
classify, and `modernc.org/libc`, which the tool labeled from the module's list of bundled
components. The Linux and Windows binaries link a slightly different set, named under Platform
differences.

Regenerate this file after any dependency change. Build the binary and run `go version -m` on it for
the module list, then run `go-licenses report .` from the repository root for the licenses, once as
is and once each with `GOOS=linux` and `GOOS=windows`.

## Apache License 2.0

- github.com/agext/levenshtein (its NOTICE: "This product includes software developed at ALRUX
  Inc. (http://www.alrux.com/).")
- github.com/go-git/go-billy/v5
- github.com/go-git/go-git/v5
- github.com/go-jose/go-jose/v4 (its json subpackage is under the BSD 3-Clause License)
- github.com/golang/groupcache
- github.com/kordloom/loomseal
- github.com/oklog/run
- github.com/pjbgf/sha1cd
- github.com/skeema/knownhosts (its NOTICE: "Copyright 2025 Skeema LLC and the Skeema Knownhosts
  authors")
- github.com/spf13/cobra
- github.com/xanzy/ssh-agent
- google.golang.org/genproto/googleapis/rpc
- google.golang.org/grpc (its NOTICE: "Copyright 2014 gRPC authors.")

## MIT License

- github.com/apparentlymart/go-textseg/v15 (its grapheme break tables are Unicode data, under the
  Unicode data files license reproduced in its LICENSE)
- github.com/dustin/go-humanize
- github.com/fatih/color
- github.com/hashicorp/go-hclog
- github.com/jackc/pgpassfile
- github.com/jackc/pgservicefile
- github.com/jackc/pgx/v5
- github.com/jackc/puddle/v2
- github.com/jbenet/go-context
- github.com/kevinburke/ssh_config
- github.com/klauspost/cpuid/v2
- github.com/mattn/go-colorable
- github.com/mattn/go-isatty
- github.com/mitchellh/go-wordwrap
- github.com/ncruces/go-strftime
- github.com/robfig/cron/v3
- github.com/sergi/go-diff
- github.com/teambition/rrule-go
- github.com/yuin/goldmark
- github.com/zclconf/go-cty
- go.uber.org/multierr
- go.uber.org/zap
- gopkg.in/yaml.v3 (MIT for the files ported from libyaml and Apache 2.0 for the rest, with its
  NOTICE: "Copyright 2011-2016 Canonical Ltd.")

## BSD 2-Clause License

- github.com/emirpasic/gods
- gopkg.in/warnings.v0

## BSD 3-Clause License

- dario.cat/mergo
- github.com/ProtonMail/go-crypto
- github.com/cloudflare/circl
- github.com/go-git/gcfg
- github.com/golang/protobuf
- github.com/google/uuid
- github.com/remyoudompheng/bigfft
- github.com/spf13/pflag
- golang.org/x/crypto
- golang.org/x/net
- golang.org/x/oauth2
- golang.org/x/sync
- golang.org/x/sys
- golang.org/x/term
- golang.org/x/text
- google.golang.org/protobuf
- modernc.org/libc (bundles additional third-party components under permissive licenses, listed
  in its LICENSE-3RD-PARTY.md)
- modernc.org/mathutil
- modernc.org/memory
- modernc.org/sqlite

## Mozilla Public License 2.0

The following components are used unmodified and remain under the MPL-2.0. Their source is available
from the URLs in their module paths.

- github.com/cyphar/filepath-securejoin (some of its files are under the BSD 3-Clause License, as
  its COPYING.md sets out)
- github.com/hashicorp/go-plugin
- github.com/hashicorp/go-version
- github.com/hashicorp/hcl/v2
- github.com/hashicorp/yamux

## Platform differences

The lists above are what the macOS binary links. The Linux binaries leave out
`github.com/ncruces/go-strftime`. The Windows binary leaves out `github.com/google/uuid` and links
two modules the others do not:

| Module                               | License            |
|--------------------------------------|--------------------|
| github.com/Microsoft/go-winio        | MIT License        |
| github.com/inconshreveable/mousetrap | Apache License 2.0 |
