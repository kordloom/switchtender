<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Go runs

A Go run compiles and runs a program with `go run`. The source is the run's command, a full
`package main` program with a `func main`. It suits a task that wants Go's type safety or an existing
Go snippet: calling an API, reshaping data, or a quick check that a shell script would strain to
express.

## What runs

The source is written to a temporary `main.go` and run with `go run`, so a whole program builds and
runs in one step. The working directory is the project checkout when the run sources a project. A dry
run runs `go vet`, which compiles and statically checks the source without executing it. The dry run
reports whether it found problems, so a clean check does not leave an empty log.

Two things to know. A program that imports only the standard library runs with no setup. Third-party
imports need a module, the same constraint `go run` places on a single file. And `go run` reports its
own exit code, one on any program failure, not the value passed to `os.Exit`, so a failing run shows
exit one with `exit status N` in the log.

## How values reach the program

- Extra vars, including survey answers and template vars, arrive as `YARDMASTER_VARS`, a JSON object:

        var vars map[string]any
        _ = json.Unmarshal([]byte(os.Getenv("YARDMASTER_VARS")), &vars)
        region, _ := vars["region"].(string)

- An `env` credential's `KEY=VALUE` lines are set in the environment, read with `os.Getenv`.
- A `token` credential is set as `YARDMASTER_TOKEN`, ready to send as a bearer token.
- Credentials attached to the run's inventory arrive the same way.

## Example

    package main

    import (
        "encoding/json"
        "net/http"
        "os"
    )

    func main() {
        var vars map[string]any
        _ = json.Unmarshal([]byte(os.Getenv("YARDMASTER_VARS")), &vars)
        service, _ := vars["service"].(string)
        req, _ := http.NewRequest(http.MethodPost, "https://api.example.com/deploy/"+service, nil)
        req.Header.Set("Authorization", "Bearer "+os.Getenv("YARDMASTER_TOKEN"))
        resp, err := http.DefaultClient.Do(req)
        if err != nil {
            os.Exit(1)
        }
        os.Stdout.WriteString(resp.Status)
    }

See also [Bash runs](tool-bash.md), [Terraform runs](tool-terraform.md),
[Python runs](tool-python.md), and the [tutorials](tutorials.md).
