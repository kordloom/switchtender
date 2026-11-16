package tfscan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// External data source declarations the cases below hang in their files.
const (
	// externalLookup declares one external data source named lookup.
	externalLookup = "data \"external\" \"lookup\" {\n  program = [\"sh\", \"-c\", \"echo {}\"]\n}\n"
	// plainOutput declares nothing that runs a program.
	plainOutput = "output \"o\" {\n  value = 1\n}\n"
)

// manifestOf returns a module manifest holding the given records, the way an init writes it.
func manifestOf(records ...string) string {
	return `{"Modules":[{"Key":"","Source":"","Dir":"."}` + strings.Join(append([]string{""},
		records...), ",") + `]}`
}

// mapFS builds a file tree from paths and contents.
func mapFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

// TestScanClassifiesWhatAPlanRuns covers the scan that decides whether a Terraform or OpenTofu
// plan can be exempted as a dry run.
//
// A plan runs the program every external data source names, with the run's credentials, so a rule
// that excluded dry runs let any program through as long as it sat in a data block. Each place one
// can sit is read, by the address a plan gives it, and anything the scan cannot read leaves the
// plan unclassified, since what was not read could declare one too.
//
//nolint:funlen,maintidx // Test function.
func TestScanClassifiesWhatAPlanRuns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Files        map[string]string
		Dir          string
		Tool         string
		WantClass    string
		WantFindings []string
		WantUnread   []string
	}{{ // Test 0: An external data source in the root module is found by its address.
		Files:        map[string]string{"main.tf": externalLookup},
		WantClass:    run.DryRunNotChangeFree,
		WantFindings: []string{"data.external.lookup runs a program during plan (main.tf line 1)"},
	}, { // Test 1: A configuration that declares none is change free.
		Files:     map[string]string{"main.tf": plainOutput, "vars.tf": "variable \"x\" {}\n"},
		WantClass: run.DryRunChangeFree,
	}, { // Test 2: One behind a local module is named by the module's address.
		Files: map[string]string{
			"main.tf":             "module \"net\" {\n  source = \"./modules/net\"\n}\n",
			"modules/net/main.tf": externalLookup,
		},
		WantClass: run.DryRunNotChangeFree,
		WantFindings: []string{
			"module.net.data.external.lookup runs a program during plan (modules/net/main.tf line 1)",
		},
	}, { // Test 3: Modules nested inside modules are followed all the way down.
		Files: map[string]string{
			"main.tf":                "module \"app\" {\n  source = \"./modules/app\"\n}\n",
			"modules/app/main.tf":    "module \"probe\" {\n  source = \"../probe\"\n}\n",
			"modules/probe/probe.tf": plainOutput + externalLookup,
		},
		WantClass: run.DryRunNotChangeFree,
		WantFindings: []string{"module.app.module.probe.data.external.lookup runs a program during " +
			"plan (modules/probe/probe.tf line 4)"},
	}, { // Test 4: A count on the data source repeats it, and the finding says so.
		Files: map[string]string{"main.tf": "data \"external\" \"each\" {\n  count = 3\n" +
			"  program = [\"sh\", \"-c\", \"echo {}\"]\n}\n"},
		WantClass: run.DryRunNotChangeFree,
		WantFindings: []string{"data.external.each runs a program during plan (main.tf line 1, " +
			"once for each instance its count makes)"},
	}, { // Test 5: A for_each on the module call repeats everything inside it.
		Files: map[string]string{
			"main.tf": "module \"net\" {\n  for_each = toset([\"a\", \"b\"])\n" +
				"  source   = \"./modules/net\"\n}\n",
			"modules/net/main.tf": externalLookup,
		},
		WantClass: run.DryRunNotChangeFree,
		WantFindings: []string{"module.net.data.external.lookup runs a program during plan " +
			"(modules/net/main.tf line 1, once for each instance of module.net)"},
	}, { // Test 6: A count of zero is not evaluated, so the block is still reported.
		Files: map[string]string{"main.tf": "data \"external\" \"off\" {\n  count = 0\n" +
			"  program = [\"true\"]\n}\n"},
		WantClass: run.DryRunNotChangeFree,
		WantFindings: []string{"data.external.off runs a program during plan (main.tf line 1, " +
			"once for each instance its count makes)"},
	}, { // Test 7: Inputs the plan only knows at run time decide nothing the scan reports.
		Files: map[string]string{"main.tf": "variable \"cmd\" {}\nvariable \"on\" {}\n" +
			"data \"external\" \"run\" {\n  count   = var.on ? 1 : 0\n  program = [var.cmd]\n}\n"},
		WantClass: run.DryRunNotChangeFree,
		WantFindings: []string{"data.external.run runs a program during plan (main.tf line 3, " +
			"once for each instance its count makes)"},
	}, { // Test 8: Unknown inputs with no external data source leave the plan change free.
		Files: map[string]string{"main.tf": "variable \"cmd\" {}\n" +
			"output \"o\" {\n  value = var.cmd\n}\n"},
		WantClass: run.DryRunChangeFree,
	}, { // Test 9: A file that does not parse leaves the plan unclassified.
		Files:     map[string]string{"main.tf": plainOutput, "broken.tf": "resource \"x\" \"y\" {\n"},
		WantClass: run.DryRunIncomplete,
		WantUnread: []string{"file \"broken.tf\" (it could not be parsed: Unclosed configuration " +
			"block, on line 1)"},
	}, { // Test 10: A module source only known when the run plans is not guessed at.
		Files: map[string]string{
			"main.tf": "variable \"src\" {\n  default = \"./modules/a\"\n}\n" +
				"module \"dyn\" {\n  source = var.src\n}\n",
			"modules/a/main.tf": plainOutput,
		},
		WantClass:  run.DryRunIncomplete,
		WantUnread: []string{"module.dyn (its source is only known when the run plans)"},
	}, { // Test 11: An interpolated source is as dynamic as a bare reference.
		Files: map[string]string{"main.tf": "locals {\n  base = \"./modules\"\n}\n" +
			"module \"dyn\" {\n  source = \"${local.base}/a\"\n}\n"},
		WantClass:  run.DryRunIncomplete,
		WantUnread: []string{"module.dyn (its source is only known when the run plans)"},
	}, { // Test 12: A registry module nobody downloaded cannot be read before the run's own init.
		Files: map[string]string{"main.tf": "module \"vpc\" {\n" +
			"  source  = \"terraform-aws-modules/vpc/aws\"\n  version = \"~> 5.0\"\n}\n"},
		WantClass: run.DryRunIncomplete,
		WantUnread: []string{"module.vpc from \"terraform-aws-modules/vpc/aws\" (" + notDownloaded +
			")"},
	}, { // Test 13: A downloaded registry module is read where init installed it.
		Files: map[string]string{
			"main.tf": "module \"vpc\" {\n  source  = \"terraform-aws-modules/vpc/aws\"\n" +
				"  version = \"~> 5.0\"\n}\n",
			".terraform/modules/modules.json": manifestOf(`{"Key":"vpc",` +
				`"Source":"registry.terraform.io/terraform-aws-modules/vpc/aws","Version":"5.1.2",` +
				`"Dir":".terraform/modules/vpc"}`),
			".terraform/modules/vpc/main.tf": externalLookup,
		},
		WantClass: run.DryRunNotChangeFree,
		WantFindings: []string{"module.vpc.data.external.lookup runs a program during plan " +
			"(.terraform/modules/vpc/main.tf line 1)"},
	}, { // Test 14: A downloaded copy of another source is replaced by the next init, so not read.
		Files: map[string]string{
			"main.tf": "module \"vpc\" {\n  source = \"acme/vpc/aws\"\n}\n",
			".terraform/modules/modules.json": manifestOf(`{"Key":"vpc",` +
				`"Source":"registry.terraform.io/terraform-aws-modules/vpc/aws","Version":"5.1.2",` +
				`"Dir":".terraform/modules/vpc"}`),
			".terraform/modules/vpc/main.tf": plainOutput,
		},
		WantClass: run.DryRunIncomplete,
		WantUnread: []string{"module.vpc from \"acme/vpc/aws\" (the copy downloaded here is of " +
			"\"registry.terraform.io/terraform-aws-modules/vpc/aws\", so the run's own init replaces " +
			"it with a copy the gate has not read)"},
	}, { // Test 15: A downloaded version the constraint no longer allows is replaced, so not read.
		Files: map[string]string{
			"main.tf": "module \"vpc\" {\n  source  = \"terraform-aws-modules/vpc/aws\"\n" +
				"  version = \">= 6.0\"\n}\n",
			".terraform/modules/modules.json": manifestOf(`{"Key":"vpc",` +
				`"Source":"registry.terraform.io/terraform-aws-modules/vpc/aws","Version":"5.1.2",` +
				`"Dir":".terraform/modules/vpc"}`),
			".terraform/modules/vpc/main.tf": plainOutput,
		},
		WantClass: run.DryRunIncomplete,
		WantUnread: []string{"module.vpc from \"terraform-aws-modules/vpc/aws\" (the copy " +
			"downloaded here is version \"5.1.2\", which \">= 6.0\" does not allow, so the run's own " +
			"init replaces it)"},
	}, { // Test 16: A downloaded module with nothing in it leaves the plan change free.
		Files: map[string]string{
			"main.tf": "module \"vpc\" {\n  source = \"terraform-aws-modules/vpc/aws//modules/sub\"\n}\n",
			".terraform/modules/modules.json": manifestOf(`{"Key":"vpc",` +
				`"Source":"registry.terraform.io/terraform-aws-modules/vpc/aws//modules/sub",` +
				`"Version":"5.1.2","Dir":".terraform/modules/vpc/modules/sub"}`),
			".terraform/modules/vpc/modules/sub/main.tf": plainOutput,
		},
		WantClass: run.DryRunChangeFree,
	}, { // Test 17: Nothing found, but a local module that is not there, is still incomplete.
		Files:      map[string]string{"main.tf": "module \"gone\" {\n  source = \"./modules/gone\"\n}\n"},
		WantClass:  run.DryRunIncomplete,
		WantUnread: []string{"module.gone from \"./modules/gone\" (not in the project)"},
	}, { // Test 18: A local module outside the tree the scan reads is not read.
		Files:      map[string]string{"main.tf": "module \"up\" {\n  source = \"../../shared\"\n}\n"},
		WantClass:  run.DryRunIncomplete,
		WantUnread: []string{"module.up from \"../../shared\" (outside the project)"},
	}, { // Test 19: A data source scoped to a check block runs during plan as well.
		Files: map[string]string{"main.tf": "check \"health\" {\n" + externalLookup +
			"  assert {\n    condition     = true\n    error_message = \"no\"\n  }\n}\n"},
		WantClass: run.DryRunNotChangeFree,
		WantFindings: []string{"data.external.lookup runs a program during plan (main.tf line 2, " +
			"in check \"health\")"},
	}, { // Test 20: The JSON syntax declares the same thing.
		Files: map[string]string{"main.tf.json": `{"data": {"external": {"lookup": ` +
			`{"program": ["sh", "-c", "echo {}"]}}}}`},
		WantClass:    run.DryRunNotChangeFree,
		WantFindings: []string{"data.external.lookup runs a program during plan (main.tf.json line 1)"},
	}, { // Test 21: An OpenTofu file is read too.
		Files: map[string]string{"main.tofu": externalLookup}, Tool: run.ToolOpenTofu,
		WantClass:    run.DryRunNotChangeFree,
		WantFindings: []string{"data.external.lookup runs a program during plan (main.tofu line 1)"},
	}, { // Test 22: A short registry address means OpenTofu's registry when OpenTofu runs.
		Files: map[string]string{
			"main.tf": "module \"vpc\" {\n  source = \"acme/vpc/aws\"\n}\n",
			".terraform/modules/modules.json": manifestOf(`{"Key":"vpc",` +
				`"Source":"registry.opentofu.org/acme/vpc/aws","Version":"1.0.0",` +
				`"Dir":".terraform/modules/vpc"}`),
			".terraform/modules/vpc/main.tf": externalLookup,
		},
		Tool:      run.ToolOpenTofu,
		WantClass: run.DryRunNotChangeFree,
		WantFindings: []string{"module.vpc.data.external.lookup runs a program during plan " +
			"(.terraform/modules/vpc/main.tf line 1)"},
	}, { // Test 23: The same copy is not what Terraform would keep, so it is not read for Terraform.
		Files: map[string]string{
			"main.tf": "module \"vpc\" {\n  source = \"acme/vpc/aws\"\n}\n",
			".terraform/modules/modules.json": manifestOf(`{"Key":"vpc",` +
				`"Source":"registry.opentofu.org/acme/vpc/aws","Version":"1.0.0",` +
				`"Dir":".terraform/modules/vpc"}`),
			".terraform/modules/vpc/main.tf": plainOutput,
		},
		WantClass: run.DryRunIncomplete,
		WantUnread: []string{"module.vpc from \"acme/vpc/aws\" (the copy downloaded here is of " +
			"\"registry.opentofu.org/acme/vpc/aws\", so the run's own init replaces it with a copy " +
			"the gate has not read)"},
	}, { // Test 24: A GitHub shorthand matches the git address init records for it.
		Files: map[string]string{
			"main.tf": "module \"x\" {\n  source = \"github.com/acme/infra//net?ref=v1\"\n}\n",
			".terraform/modules/modules.json": manifestOf(`{"Key":"x",` +
				`"Source":"git::https://github.com/acme/infra.git//net?ref=v1",` +
				`"Dir":".terraform/modules/x/net"}`),
			".terraform/modules/x/net/main.tf": externalLookup,
		},
		WantClass: run.DryRunNotChangeFree,
		WantFindings: []string{"module.x.data.external.lookup runs a program during plan " +
			"(.terraform/modules/x/net/main.tf line 1)"},
	}, { // Test 25: An ephemeral external resource opens during plan, so it is reported as well.
		Files:     map[string]string{"main.tf": "ephemeral \"external\" \"token\" {\n}\n"},
		WantClass: run.DryRunNotChangeFree,
		WantFindings: []string{
			"ephemeral.external.token may run a program during plan (main.tf line 1)",
		},
	}, { // Test 26: An override that sets no source merges into a call declared elsewhere.
		Files: map[string]string{
			"main.tf":             "module \"net\" {\n  source = \"./modules/net\"\n}\n",
			"override.tf":         "module \"net\" {\n  count = 2\n}\n",
			"modules/net/main.tf": plainOutput,
		},
		WantClass: run.DryRunChangeFree,
	}, { // Test 27: A file the tools skip as an editor leftover is skipped here too.
		Files: map[string]string{
			"main.tf": plainOutput, ".#main.tf": "not hcl {", "main.tf~": externalLookup,
		},
		WantClass: run.DryRunChangeFree,
	}, { // Test 28: Data sources of any other type run no program the scan looks for.
		Files: map[string]string{"main.tf": "data \"http\" \"page\" {\n  url = \"https://x\"\n}\n" +
			"data \"aws_ami\" \"x\" {\n}\n"},
		WantClass: run.DryRunChangeFree,
	}, { // Test 29: The configuration is read in the directory the run names.
		Files: map[string]string{
			"infra/prod/main.tf": "module \"base\" {\n  source = \"../../modules/base\"\n}\n",
			"modules/base/x.tf":  externalLookup,
		},
		Dir:       "infra/prod",
		WantClass: run.DryRunNotChangeFree,
		WantFindings: []string{"module.base.data.external.lookup runs a program during plan " +
			"(modules/base/x.tf line 1)"},
	}, { // Test 30: A working directory that is not there is unread, never change free.
		Files:      map[string]string{"main.tf": plainOutput},
		Dir:        "infra/missing",
		WantClass:  run.DryRunIncomplete,
		WantUnread: []string{"the working directory \"infra/missing\" (not in the project)"},
	}, { // Test 31: A call with no source names nothing to read.
		Files:      map[string]string{"main.tf": "module \"odd\" {\n}\n"},
		WantClass:  run.DryRunIncomplete,
		WantUnread: []string{"module.odd (it names no source)"},
	}, { // Test 32: A version only known when the run plans leaves the module unread.
		Files: map[string]string{
			"main.tf": "variable \"v\" {}\nmodule \"vpc\" {\n" +
				"  source  = \"terraform-aws-modules/vpc/aws\"\n  version = var.v\n}\n",
			".terraform/modules/modules.json": manifestOf(`{"Key":"vpc",` +
				`"Source":"registry.terraform.io/terraform-aws-modules/vpc/aws","Version":"5.1.2",` +
				`"Dir":".terraform/modules/vpc"}`),
			".terraform/modules/vpc/main.tf": plainOutput,
		},
		WantClass: run.DryRunIncomplete,
		WantUnread: []string{"module.vpc from \"terraform-aws-modules/vpc/aws\" (its version is " +
			"only known when the run plans)"},
	}, { // Test 33: A downloaded copy recorded by an absolute path is outside the tree.
		Files: map[string]string{
			"main.tf": "module \"vpc\" {\n  source = \"acme/vpc/aws\"\n}\n",
			".terraform/modules/modules.json": manifestOf(`{"Key":"vpc",` +
				`"Source":"registry.terraform.io/acme/vpc/aws","Version":"1.0.0",` +
				`"Dir":"/.terraform/modules/vpc"}`),
			".terraform/modules/vpc/main.tf": plainOutput,
		},
		WantClass: run.DryRunIncomplete,
		WantUnread: []string{"module.vpc from \"acme/vpc/aws\" (its downloaded copy is outside " +
			"the project)"},
	}, { // Test 34: A manifest that is not JSON leaves every downloaded module unread.
		Files: map[string]string{
			"main.tf":                         "module \"vpc\" {\n  source = \"acme/vpc/aws\"\n}\n",
			".terraform/modules/modules.json": "not json",
		},
		WantClass: run.DryRunIncomplete,
		WantUnread: []string{"module.vpc from \"acme/vpc/aws\" (the module manifest " +
			"\".terraform/modules/modules.json\" could not be parsed)"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := test.Dir
			if dir == "" {
				dir = "."
			}
			// The .terraform entries in these cases stand for a tree the gate itself downloaded, so the
			// scan is told to read its manifest. A committed .terraform is never trusted, which
			// TestScanIgnoresACommittedModuleManifest covers.
			got := Scan(mapFS(test.Files), dir, Options{Tool: test.Tool, Place: "the project",
				TrustModuleManifest: true})
			if got.Classification != test.WantClass {
				t.Errorf("Classification = %q, want %q (findings %q, unread %q)", got.Classification,
					test.WantClass, got.Findings, got.Unread)
			}
			if diff := cmp.Diff(test.WantFindings, got.Findings, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Findings mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantUnread, got.Unread, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unread mismatch (-want +got):\n%s", diff)
			}
			if got.Scanner != Scanner || got.Version != Version {
				t.Errorf("scanner = %s v%d, want %s v%d", got.Scanner, got.Version, Scanner, Version)
			}
			wantTool := run.ToolTerraform
			if test.Tool != "" {
				wantTool = test.Tool
			}
			if got.Tool != wantTool {
				t.Errorf("Tool = %q, want %q", got.Tool, wantTool)
			}
		})
	}
}

// TestScanNamesWhatItExamined covers the evidence half of a scan: which files it read, so an
// auditor can hold a classification against the exact configuration it was made from.
func TestScanNamesWhatItExamined(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"infra/main.tf":      "module \"net\" {\n  source = \"./net\"\n}\n",
		"infra/vars.tf.json": `{"variable": {"x": {}}}`,
		"infra/net/main.tf":  plainOutput,
		"infra/README.md":    "not configuration",
		"infra/.hidden.tf":   externalLookup,
		"elsewhere/main.tf":  externalLookup,
	}
	got := Scan(mapFS(files), "infra", Options{Tool: run.ToolTerraform, Place: "the project"})
	want := []string{"infra/main.tf", "infra/net/main.tf", "infra/vars.tf.json"}
	if diff := cmp.Diff(want, got.Inputs); diff != "" {
		t.Errorf("Inputs mismatch (-want +got):\n%s", diff)
	}
	if got.Classification != run.DryRunChangeFree {
		t.Errorf("Classification = %q, want change_free: %q %q", got.Classification, got.Findings,
			got.Unread)
	}
}

// TestScanOfAHostileTreeEnds covers the bounds. A scan runs on the submit path, so a configuration
// that calls itself, or fans out, must end, and what it did not read must cost the classification.
func TestScanOfAHostileTreeEnds(t *testing.T) {
	t.Parallel()
	fan := map[string]string{}
	for i := range 9 {
		next := fmt.Sprintf("../m%d", i+1)
		if i == 0 {
			next = "./m1"
		}
		body := fmt.Sprintf("module \"a\" {\n  source = %q\n}\nmodule \"b\" {\n  source = %q\n}\n",
			next, next)
		if i == 0 {
			fan["main.tf"] = body
			continue
		}
		fan[fmt.Sprintf("m%d/main.tf", i)] = body
	}
	fan["m9/main.tf"] = plainOutput
	big := map[string]string{"main.tf": plainOutput + strings.Repeat("#", maxFileBytes)}
	tests := []struct {
		Files    map[string]string
		WantText string
	}{{ // Test 0: A module that calls itself stops at the depth bound.
		Files:    map[string]string{"main.tf": "module \"self\" {\n  source = \"./\"\n}\n"},
		WantText: fmt.Sprintf("nested more than %d deep", maxDepth),
	}, { // Test 1: Two calls per level multiply until the call bound stops them.
		Files:    fan,
		WantText: fmt.Sprintf("past the %d module calls one scan follows", maxCalls),
	}, { // Test 2: A file past the size one file may be is not read.
		Files:    big,
		WantText: fmt.Sprintf("larger than the %d MiB one file may be", maxFileBytes>>20),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := Scan(mapFS(test.Files), ".", Options{Tool: run.ToolTerraform, Place: "the project"})
			if got.ChangeFree() {
				t.Fatalf("a scan that stopped at its bounds classified the plan as change free")
			}
			if !strings.Contains(strings.Join(got.Unread, "\n"), test.WantText) {
				t.Errorf("Unread = %q, want an entry mentioning %q", got.Unread, test.WantText)
			}
		})
	}
}

// TestRegistryAndRemoteSourcesNormalize pins how a configured source is compared with the one an
// init recorded. A form this reads as the same source is read from the downloaded copy, so a wrong
// match reads a copy the run's own init would replace, which is the one way this could fail open.
func TestRegistryAndRemoteSourcesNormalize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Source   string
		Recorded string
		Host     string
		Want     bool
	}{{ // Test 0: A short registry address gains the default host.
		Source: "hashicorp/consul/aws", Recorded: "registry.terraform.io/hashicorp/consul/aws",
		Host: terraformRegistry, Want: true,
	}, { // Test 1: A subdirectory stays after the package.
		Source: "hashicorp/consul/aws//modules/x", Host: terraformRegistry, Want: true,
		Recorded: "registry.terraform.io/hashicorp/consul/aws//modules/x",
	}, { // Test 2: An explicit host is kept, lower cased.
		Source: "App.Terraform.io/acme/net/aws", Recorded: "app.terraform.io/acme/net/aws",
		Host: terraformRegistry, Want: true,
	}, { // Test 3: Another registry's copy is not this one.
		Source: "hashicorp/consul/aws", Recorded: "registry.opentofu.org/hashicorp/consul/aws",
		Host: terraformRegistry, Want: false,
	}, { // Test 4: A GitHub shorthand is the git address it stands for.
		Source: "github.com/acme/infra", Recorded: "git::https://github.com/acme/infra.git",
		Host: terraformRegistry, Want: true,
	}, { // Test 5: A forced git address matches only as written.
		Source: "git::https://example.com/net.git?ref=v1", Host: terraformRegistry, Want: true,
		Recorded: "git::https://example.com/net.git?ref=v1",
	}, { // Test 6: A different ref is a different source.
		Source: "git::https://example.com/net.git?ref=v2", Host: terraformRegistry, Want: false,
		Recorded: "git::https://example.com/net.git?ref=v1",
	}, { // Test 7: GitHub is never a registry host.
		Source: "github.com/acme/net/aws", Recorded: "github.com/acme/net/aws",
		Host: terraformRegistry, Want: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := &scan{registry: test.Host}
			if got := s.sameSource(test.Source, test.Recorded); got != test.Want {
				t.Errorf("sameSource(%q, %q) = %v, want %v", test.Source, test.Recorded, got, test.Want)
			}
		})
	}
}

// TestScanIgnoresACommittedModuleManifest is the negative control for distrusting a committed
// .terraform. The same tree, scanned with and without TrustModuleManifest, reads the planted copy
// only when it is trusted: a committed manifest pointing a registry module at a clean copy must not
// let that module read as change free, since the run's own init downloads the real one.
func TestScanIgnoresACommittedModuleManifest(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"main.tf": "module \"vpc\" {\n  source  = \"terraform-aws-modules/vpc/aws\"\n" +
			"  version = \"~> 5.0\"\n}\n",
		".terraform/modules/modules.json": manifestOf(`{"Key":"vpc",` +
			`"Source":"registry.terraform.io/terraform-aws-modules/vpc/aws","Version":"5.1.2",` +
			`"Dir":".terraform/modules/vpc"}`),
		".terraform/modules/vpc/main.tf": plainOutput,
	}
	// Trusted, as the gate's own downloaded tree is: the planted copy is read and classifies.
	trusted := Scan(mapFS(files), ".", Options{Tool: run.ToolTerraform, Place: "the project",
		TrustModuleManifest: true})
	if trusted.Classification != run.DryRunChangeFree {
		t.Fatalf("with the manifest trusted, classification = %q, want change_free (control)",
			trusted.Classification)
	}
	// Distrusted, as a committed tree is: the module reads as not downloaded, so the gate downloads
	// it rather than trusting the committed copy.
	got := Scan(mapFS(files), ".", Options{Tool: run.ToolTerraform, Place: "the project"})
	if got.Classification != run.DryRunIncomplete {
		t.Errorf("a committed manifest was trusted: classification = %q, want incomplete so the "+
			"module goes through the gate's own download (findings %q, unread %q)",
			got.Classification, got.Findings, got.Unread)
	}
}

// Configurations the provider set cases below build on.
const (
	// awsRequired requires the AWS provider at a version constraint.
	awsRequired = "terraform {\n  required_providers {\n    aws = {\n" +
		"      source  = \"hashicorp/aws\"\n      version = \"~> 5.0\"\n    }\n  }\n}\n"
	// awsConfigured configures the AWS provider.
	awsConfigured = "provider \"aws\" {\n  region = \"us-east-1\"\n}\n"
	// awsInstance declares one AWS resource.
	awsInstance = "resource \"aws_instance\" \"web\" {\n  ami = \"ami-1\"\n}\n"
)

// vpcModule returns a configuration calling the registry VPC module at version constraint.
func vpcModule(constraint string) string {
	return "module \"vpc\" {\n  source  = \"terraform-aws-modules/vpc/aws\"\n  version = \"" +
		constraint + "\"\n}\n"
}

// lockEntry returns the entry a lock file holding content adds to a provider set.
func lockEntry(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "lock file sha256 " + hex.EncodeToString(sum[:])
}

// TestProvidersReadsTheProviderSet covers the provider set a configuration installs: every place a
// provider is required, implied, configured, or pinned, read through local modules, and a set that
// cannot be known before init reported as unknown rather than guessed at.
//
//nolint:funlen // Test function.
func TestProvidersReadsTheProviderSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Files       map[string]string
		Dir         string
		WantEntries []string
		Want        error
	}{{ // Test 0: A required provider, its configuration, and a resource of its type.
		Files: map[string]string{"main.tf": awsRequired + awsConfigured + awsInstance},
		WantEntries: []string{"local name aws", "provider aws",
			`required_providers aws source "hashicorp/aws" version "~> 5.0"`},
	}, { // Test 1: The older bare version string is read as the constraint.
		Files: map[string]string{"versions.tf": "terraform {\n  required_providers {\n" +
			"    aws = \"~> 3.0\"\n  }\n}\n"},
		WantEntries: []string{`required_providers aws source "" version "~> 3.0"`},
	}, { // Test 2: An aliased provider block and a provider meta-argument are both read.
		Files: map[string]string{"main.tf": "provider \"aws\" {\n  alias = \"west\"\n}\n" +
			"resource \"random_id\" \"x\" {\n  provider = google-beta\n}\n"},
		WantEntries: []string{"local name google-beta", "local name random",
			`provider aws alias "west"`},
	}, { // Test 3: A data source in a check block and a local module's types are read.
		Files: map[string]string{
			"main.tf": "module \"net\" {\n  source = \"./modules/net\"\n}\n" +
				"check \"c\" {\n  data \"dns_a_record_set\" \"y\" {\n    host = \"x\"\n  }\n}\n",
			"modules/net/main.tf": "data \"http\" \"x\" {\n  url = \"https://example.com\"\n}\n",
		},
		WantEntries: []string{"local name dns", "local name http"},
	}, { // Test 4: The lock file beside the root module is part of the set.
		Files:       map[string]string{"main.tf": awsInstance, ".terraform.lock.hcl": "pinned\n"},
		WantEntries: []string{"local name aws", lockEntry("pinned\n")},
	}, { // Test 5: JSON configuration is read the same way.
		Files: map[string]string{"main.tf.json": `{"terraform":{"required_providers":{"aws":` +
			`{"source":"hashicorp/aws","version":"5.1.0"}}},"resource":{"aws_s3_bucket":{"b":{}}}}`},
		WantEntries: []string{"local name aws",
			`required_providers aws source "hashicorp/aws" version "5.1.0"`},
	}, { // Test 6: The working directory is read inside the tree.
		Files: map[string]string{"infra/main.tf": awsInstance,
			"main.tf": "resource \"x_y\" \"z\" {}\n"},
		Dir:         "infra",
		WantEntries: []string{"local name aws"},
	}, { // Test 7: A configuration with no providers has an empty set.
		Files: map[string]string{"main.tf": plainOutput},
	}, { // Test 8: A registry module call is an entry by its source and version constraint.
		Files: map[string]string{"main.tf": vpcModule("~> 5.0")},
		WantEntries: []string{
			`module source "terraform-aws-modules/vpc/aws" version "~> 5.0"`},
	}, { // Test 9: A remote module call with no version is an entry by its source.
		Files: map[string]string{"main.tf": "module \"m\" {\n  source = " +
			"\"git::https://example.com/m.git\"\n}\n"},
		WantEntries: []string{`module source "git::https://example.com/m.git" version ""`},
	}, { // Test 10: A registry module version only known when the run plans leaves the set unknown.
		Files: map[string]string{"main.tf": "module \"vpc\" {\n  source = " +
			"\"terraform-aws-modules/vpc/aws\"\n  version = var.v\n}\n"},
		Want: ErrProvidersUnknown,
	}, { // Test 11: A module source only known when the run plans cannot be followed.
		Files: map[string]string{"main.tf": "module \"m\" {\n  source = var.where\n}\n"},
		Want:  ErrProvidersUnknown,
	}, { // Test 12: A file that does not parse leaves the set unknown.
		Files: map[string]string{"main.tf": "resource \"aws_instance\" {{{\n"},
		Want:  ErrProvidersUnknown,
	}, { // Test 13: A required provider whose source is not a literal leaves the set unknown.
		Files: map[string]string{"main.tf": "terraform {\n  required_providers {\n" +
			"    aws = { source = local.src }\n  }\n}\n"},
		Want: ErrProvidersUnknown,
	}, { // Test 14: A working directory that is not in the tree leaves the set unknown.
		Files: map[string]string{"main.tf": awsInstance},
		Dir:   "missing",
		Want:  ErrProvidersUnknown,
	}, { // Test 15: A local module that is not in the tree leaves the set unknown.
		Files: map[string]string{"main.tf": "module \"m\" {\n  source = \"./gone\"\n}\n"},
		Want:  ErrProvidersUnknown,
	}, { // Test 16: An action block's type and provider meta-argument are read.
		Files: map[string]string{"main.tf": "action \"local_command\" \"x\" {\n" +
			"  provider = awscc\n}\n"},
		WantEntries: []string{"local name awscc", "local name local"},
	}, { // Test 17: A provider mirror beside the root module leaves the set unknown.
		Files: map[string]string{"main.tf": awsInstance, "terraform.d/plugins/registry." +
			"terraform.io/hashicorp/aws/5.0.0/linux_amd64/terraform-provider-aws_v5.0.0": "bin"},
		Want: ErrProvidersUnknown,
	}, { // Test 18: A provider mirror outside the working directory is not the run's.
		Files: map[string]string{"infra/main.tf": awsInstance,
			"terraform.d/plugins/registry.terraform.io/hashicorp/aws/5.0.0/x/p": "bin"},
		Dir:         "infra",
		WantEntries: []string{"local name aws"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := test.Dir
			if dir == "" {
				dir = "."
			}
			got, err := Providers(mapFS(test.Files), dir)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Providers() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantEntries, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("entries mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// treeOf returns a configuration tree of main.tf holding main, net.tf holding net, and a lock file
// holding lock.
func treeOf(main, net, lock string) map[string]string {
	return map[string]string{"main.tf": main, "net.tf": net, ".terraform.lock.hcl": lock}
}

// TestProvidersTellsAChangedHead compares a pull request's head against the commit the template
// runs today, the way the review does: a head whose provider set differs, or cannot be known,
// counts as changed, and one that only changes a resource's arguments, or keeps the same module
// call, does not.
func TestProvidersTellsAChangedHead(t *testing.T) {
	t.Parallel()
	const lock = "aws 5.0.0\n"
	main := awsRequired + awsConfigured + awsInstance
	net := vpcModule("~> 5.0")
	tests := []struct {
		Head        map[string]string
		WantChanged bool
	}{{ // Test 0: The head adds a required_providers entry.
		Head: treeOf(strings.Replace(main, "    aws = {",
			"    tls = {\n      source = \"hashicorp/tls\"\n    }\n    aws = {", 1), net, lock),
		WantChanged: true,
	}, { // Test 1: The head adds a resource of a new provider type with no required_providers change.
		Head:        treeOf(main+"resource \"google_compute_instance\" \"vm\" {}\n", net, lock),
		WantChanged: true,
	}, { // Test 2: The head changes the lock file.
		Head:        treeOf(main, net, "aws 5.9.9\n"),
		WantChanged: true,
	}, { // Test 3: The head adds a registry module.
		Head: treeOf(main+"module \"dns\" {\n  source = \"terraform-aws-modules/route53/aws\"\n}\n",
			net, lock),
		WantChanged: true,
	}, { // Test 4: The head changes a registry module's version constraint.
		Head:        treeOf(main, vpcModule("~> 6.0"), lock),
		WantChanged: true,
	}, { // Test 5: The head calls a module whose source is only known when the run plans.
		Head:        treeOf(main+"module \"m\" {\n  source = var.where\n}\n", net, lock),
		WantChanged: true,
	}, { // Test 6: The head changes only a resource's arguments.
		Head: treeOf(strings.Replace(main, "ami-1", "ami-2", 1), net, lock),
	}, { // Test 7: The head keeps the same registry module call.
		Head: treeOf(main, net, lock),
	}, { // Test 8: The head adds an action of a new provider type.
		Head:        treeOf(main+"action \"local_command\" \"x\" {}\n", net, lock),
		WantChanged: true,
	}, { // Test 9: The head adds a provider binary to the local mirror.
		Head: func() map[string]string {
			tree := treeOf(main, net, lock)
			tree["terraform.d/plugins/registry.terraform.io/hashicorp/aws/5.0.0/linux_amd64/"+
				"terraform-provider-aws_v5.0.0"] = "bin"
			return tree
		}(),
		WantChanged: true,
	}}
	want, err := Providers(mapFS(treeOf(main, net, lock)), ".")
	if err != nil {
		t.Fatalf("Providers(base) error = %v", err)
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := Providers(mapFS(test.Head), ".")
			changed := err != nil || !slices.Equal(want, got)
			if diff := cmp.Diff(test.WantChanged, changed); diff != "" {
				t.Errorf("changed mismatch (-want +got):\n%s\nbase %q, head %q, error %v", diff,
					want, got, err)
			}
		})
	}
}
