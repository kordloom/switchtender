<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Extend Yardmaster in Go

Yardmaster's seams are public. Import one package, `github.com/dcadolph/yardmaster/sdk`, register
what you built, and run it either way: compiled into the server binary, or dropped next to a stock
release binary as a plugin the server loads at startup. A registered tool submits, validates,
executes, and audits like a built-in. The SDK covers four kinds of extension:

- **Execution tools.** `RegisterTool` adds a tool beside ansible, bash, terraform, opentofu,
  python, powershell, and go. Your runner receives the run's command and streams output that lands
  in the run log, masked and audited like any other run.
- **AI providers.** `RegisterAIProvider` adds a model backend beside ollama, anthropic, and openai.
  Advisory only: a provider produces text a human reads, never an action, so it stays out of the
  execution path.
- **Notification channels.** `RegisterNotifier` delivers every terminal top-level run to your
  channel, beside the built-in webhook, Slack, and email delivery. Extra vars are redacted before
  delivery, so survey answers and template vars that can carry secrets never leave the server.
- **Secret engines.** `RegisterSecretSource` adds a static engine such as AWS Secrets Manager or
  1Password. `RegisterDynamicSecretSource` adds a dynamic engine that mints a short-lived
  credential on each read and returns a lease Yardmaster revokes when the run ends.

## The rules

Register from an `init` function or from `main` before the server starts. Registries are read
while serving and never written after, so registration is a startup step, not a runtime one. An
empty, duplicate, or reserved name panics at boot, where the mistake is cheap, instead of failing
on first use.

An extension loads one of two ways. Compile it into the binary for a single static build with no
version skew. Or ship it as a plugin binary in the server's `--plugins-dir` for a stock release
binary that loads extensions at startup. Same seams, same registration, same behavior at run time.

## A complete extension

Two files make a Yardmaster with a custom `hello` tool.

`go.mod`:

    module example.com/yardmaster-hello

    go 1.26.4

    require github.com/dcadolph/yardmaster v1.7.0

`main.go`:

    // Package main builds a Yardmaster server with a custom tool compiled in through the SDK.
    package main

    import (
        "context"
        "fmt"
        "io"

        "github.com/dcadolph/yardmaster/cmd"
        "github.com/dcadolph/yardmaster/sdk"
    )

    // init registers the tool before the server starts.
    func init() {
        sdk.RegisterTool("hello", sdk.ToolRunnerFunc(
            func(_ context.Context, spec sdk.ToolSpec, out io.Writer) (sdk.ToolResult, error) {
                fmt.Fprintf(out, "hello from an external plugin: %s\n", spec.Command)
                return sdk.ToolResult{ExitCode: 0}, nil
            }))
    }

    // main runs the stock Yardmaster CLI with the extension compiled in.
    func main() {
        cmd.Execute(nil)
    }

Build it, run it, and submit a run that names the new tool:

    go mod tidy && go build -o yardmaster-hello .
    ./yardmaster-hello serve --addr :8080 --db yard.db

    curl -s -X POST localhost:8080/v1/runs \
      -H 'Content-Type: application/json' \
      -d '{"tool":"hello","command":"world"}'

The run is accepted, executed, and audited like any built-in tool, and its log holds the runner's
output:

    curl -s localhost:8080/v1/runs/<id>/logs
    hello from an external plugin: world

The binary keeps every stock command: `serve`, `worker`, `desktop`, `demo`, `import`, `token`,
`user`, and `audit`. Passing `nil` to `cmd.Execute` skips the embedded documentation pages in the
UI. Embed your own file tree there to serve them.

## Ship it as a plugin binary

The same extension runs as its own process, loaded by a stock Yardmaster release binary. Swap the
main for `plugin.Serve` and build:

    // Command helloplugin serves the hello tool as a Yardmaster plugin.
    package main

    import (
        "context"
        "fmt"
        "io"

        "github.com/dcadolph/yardmaster/sdk"
        "github.com/dcadolph/yardmaster/sdk/plugin"
    )

    // main serves the extension as a plugin process.
    func main() {
        plugin.Serve(&plugin.Extension{
            Tools: map[string]sdk.ToolRunner{
                "hello": sdk.ToolRunnerFunc(
                    func(_ context.Context, spec sdk.ToolSpec, out io.Writer) (sdk.ToolResult, error) {
                        fmt.Fprintf(out, "hello from a plugin process: %s\n", spec.Command)
                        return sdk.ToolResult{ExitCode: 0}, nil
                    }),
            },
        })
    }

Drop the binary in a directory and point the server at it:

    go build -o plugins/hello-plugin .
    yardmaster serve --plugins-dir ./plugins

At startup the server launches each executable in the directory, asks what it provides, and
registers every seam it declares. One plugin serves any mix of tools, notifiers, AI providers, and
secret engines from a single `Extension`. The process speaks gRPC over a local socket with mutual
TLS, supervised by the server: it starts with the server and exits with it. The worker command
takes the same flag, so a plugged-in tool runs wherever runs execute.

A plugin that fails to launch or describe itself is logged and skipped, so one broken binary does
not take the server down. A name that collides with a built-in or another plugin stops the server
at startup with a clear message. Compiling in and plugging in register the same way and behave the
same at run time. Pick per extension: compile in for one static artifact, plug in for extending a
release binary you did not build.

For a working example, the official
[yardmaster-plugins](https://github.com/dcadolph/yardmaster-plugins) repo ships
`yardmaster-notify`, one plugin binary that delivers runs to Discord, ntfy, and Microsoft Teams.
It doubles as the template for writing your own.

## The seams in detail

### Execution tools

A `ToolRunner` receives the run as a `ToolSpec` and a writer for its output. `Command` carries the
tool's input, the same field the bash and python tools read their script from. `DryRun` asks for
the tool's no-change mode. `ExtraVars`, `Env`, and `Dir` carry the run's variables, environment,
and working directory. Return the process exit code in `ToolResult`. Return an error only when the
tool could not be launched or supervised.

### Notification channels

A `Notifier` receives each top-level run once it reaches a terminal state: succeeded, failed,
canceled, interrupted, or rejected. Delivery happens off the executor path with a bounded timeout,
and a failed delivery is logged and dropped, so a slow channel cannot stall runs.

### AI providers

An `AIProviderFactory` builds a provider from three settings: the model name, the endpoint URL,
and an API key. Validate what you require and fail at startup, not on first use. The provider's
one method turns a system instruction and a user prompt into text.

### Secret engines

A `SecretResolver` fetches a value from a source's config at run time. A `SecretMinter` does the
same for a dynamic engine and also returns a lease built with `NewSecretLease`, naming the engine
and capturing how to revoke the minted credential. Pass a nil revoke func when the secret only
expires on the engine's own TTL.

See also the [secrets guide](secrets.md), [Bash runs](tool-bash.md) for how a tool's command and
variables behave, and the [HTTP API](api.md) for submitting runs to your new tool.
