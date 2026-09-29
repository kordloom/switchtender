package importer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/credential"
)

// inventoryFromVars maps an AWX inventory carrying the group and inventory-wide variables given, so
// each case below varies only the part it is about.
func inventoryFromVars(t *testing.T, groupVars, children, allVars string) *Plan {
	t.Helper()
	doc := fmt.Sprintf(`{"inventory": [{
      "name": "prod",
      "hosts": [{"name": "web01"}],
      "groups": [{"name": "web", "hosts": [{"name": "web02"}],
        "variables": %s, "children": %s}],
      "variables": %s
    }]}`, groupVars, children, allVars)
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v\ndocument:\n%s", err, doc)
	}
	return plan
}

// TestGroupVarsRefuseAKeyAnInventoryParserWouldReadAsSomethingElse pins that the same rule the host
// line applies is applied inside a [name:vars] section. A variable key holding whitespace or an
// inventory metacharacter would be read as a new section or as further directives, so it is dropped
// and reported rather than written.
func TestGroupVarsRefuseAKeyAnInventoryParserWouldReadAsSomethingElse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Vars        string
		WantKept    []string
		WantDropped []string
	}{
		{Name: "ordinary", Vars: `{"http_port": 80}`,
			WantKept: []string{"http_port=80"}}, // Test 0.
		{Name: "key with a space", Vars: `{"bad key": "v", "good": "v"}`,
			WantKept: []string{"good=v"}, WantDropped: []string{"bad key"}}, // Test 1.
		{Name: "key with an equals", Vars: `{"a=b": "v", "good": "v"}`,
			WantKept: []string{"good=v"}, WantDropped: []string{"a=b"}}, // Test 2.
		{Name: "key with a bracket", Vars: `{"[all]": "v", "good": "v"}`,
			WantKept: []string{"good=v"}, WantDropped: []string{"[all]"}}, // Test 3.
		{Name: "key with a hash", Vars: `{"a#b": "v", "good": "v"}`,
			WantKept: []string{"good=v"}, WantDropped: []string{"a#b"}}, // Test 4.
		{Name: "key with a newline", Vars: `{"a\nb": "v", "good": "v"}`,
			WantKept: []string{"good=v"}, WantDropped: []string{`a\\nb`}}, // Test 5.
		{Name: "value with a newline", Vars: `{"bad": "one\ntwo", "good": "v"}`,
			WantKept: []string{"good=v"}, WantDropped: []string{"control character"}}, // Test 6.
		{Name: "value with a space is quoted", Vars: `{"note": "with space"}`,
			WantKept: []string{`note="with space"`}}, // Test 7.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan := inventoryFromVars(t, test.Vars, `[]`, `{}`)
			content := plan.Inventories[0].Content
			for _, want := range test.WantKept {
				if !strings.Contains(content, want) {
					t.Errorf("content is missing %q:\n%s", want, content)
				}
			}
			for _, dropped := range test.WantDropped {
				if _, ok := warningContaining(t, plan.Warnings, "web:vars", "dropped"); !ok {
					t.Errorf("a dropped group variable was not reported.\nwarnings: %v",
						plan.Warnings)
				}
				if _, ok := warningContaining(t, plan.Warnings, dropped); !ok {
					t.Errorf("the report does not name %q.\nwarnings: %v", dropped, plan.Warnings)
				}
			}
		})
	}
}

// TestInventoryWideVarsApplyTheSameRule pins that [all:vars] is guarded like every other section.
// An inventory-wide variable belongs to every host in it, so one that redirects a connection is the
// widest reach an unchecked value has.
func TestInventoryWideVarsApplyTheSameRule(t *testing.T) {
	t.Parallel()
	plan := inventoryFromVars(t, `{}`, `[]`,
		`{"ansible_user": "deploy", "bad key": "x", "carriage": "a\rb"}`)
	content := plan.Inventories[0].Content
	if !strings.Contains(content, "[all:vars]\nansible_user=deploy") {
		t.Errorf("the good inventory-wide variable is missing:\n%s", content)
	}
	if strings.Contains(content, "bad key") {
		t.Errorf("a variable key holding a space was written:\n%s", content)
	}
	if strings.Contains(content, "carriage") {
		t.Errorf("a variable value holding a control character was written:\n%s", content)
	}
	if _, ok := warningContaining(t, plan.Warnings, "all:vars", "bad key"); !ok {
		t.Errorf("the dropped key was not reported.\nwarnings: %v", plan.Warnings)
	}
	if _, ok := warningContaining(t, plan.Warnings, "all:vars", "control character"); !ok {
		t.Errorf("the dropped value was not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestChildGroupNamesAreRefusedRatherThanWritten pins the [name:children] section. A child name that
// would be read as a new section header lets an export write inventory directives of its own, and a
// silently altered group membership changes which hosts a play targeting the parent reaches.
func TestChildGroupNamesAreRefusedRatherThanWritten(t *testing.T) {
	t.Parallel()
	plan := inventoryFromVars(t, `{}`, `["canary", "bad child", "[all]", "a=b"]`, `{}`)
	content := plan.Inventories[0].Content
	if !strings.Contains(content, "[web:children]\ncanary\n") {
		t.Errorf("the good child group is missing:\n%s", content)
	}
	for _, unwanted := range []string{"bad child", "[all]", "a=b"} {
		if strings.Contains(content, unwanted) {
			t.Errorf("the unsafe child name %q was written:\n%s", unwanted, content)
		}
	}
	for _, name := range []string{"bad child", "[all]", "a=b"} {
		if _, ok := warningContaining(t, plan.Warnings, "child group", name); !ok {
			t.Errorf("the dropped child %q was not reported.\nwarnings: %v", name, plan.Warnings)
		}
	}
}

// TestAGroupWithOnlyUnsafeChildrenStillWritesItsSection pins that dropping every child leaves an
// empty section rather than a malformed one. A half-written directive is what makes the rest of an
// inventory unreadable.
func TestAGroupWithOnlyUnsafeChildrenStillWritesItsSection(t *testing.T) {
	t.Parallel()
	plan := inventoryFromVars(t, `{}`, `["bad child"]`, `{}`)
	content := plan.Inventories[0].Content
	if strings.Contains(content, "bad child") {
		t.Errorf("the unsafe child was written:\n%s", content)
	}
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "[") {
			continue
		}
		if strings.HasPrefix(trimmed, "web01") || strings.HasPrefix(trimmed, "web02") {
			continue
		}
		t.Errorf("an unexpected line survived in the inventory: %q\n%s", trimmed, content)
	}
}

// TestGroupSectionsAreOmittedWhenThereIsNothingToWrite pins that a group with no variables and no
// children produces no empty [name:vars] or [name:children] header. An empty section reads to a
// person as a group that was configured and is not.
func TestGroupSectionsAreOmittedWhenThereIsNothingToWrite(t *testing.T) {
	t.Parallel()
	plan := inventoryFromVars(t, `{}`, `[]`, `{}`)
	content := plan.Inventories[0].Content
	for _, unwanted := range []string{"[web:vars]", "[web:children]", "[all:vars]"} {
		if strings.Contains(content, unwanted) {
			t.Errorf("an empty %s section was written:\n%s", unwanted, content)
		}
	}
	want := "web01\n\n[web]\nweb02\n"
	if diff := cmp.Diff(want, content); diff != "" {
		t.Errorf("content mismatch (-want +got):\n%s", diff)
	}
}

// TestAnInventoryOfOnlyGroupsStillEndsWithOneNewline pins the trailing whitespace the renderer
// promises. An inventory file with stray blank structure still parses, but the exact shape is what
// makes a re-import and a hand edit produce the same file.
func TestAnInventoryOfOnlyGroupsStillEndsWithOneNewline(t *testing.T) {
	t.Parallel()
	const doc = `{"inventory": [{"name": "prod",
      "groups": [{"name": "web", "hosts": [{"name": "web01"}]}]}]}`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	content := plan.Inventories[0].Content
	if diff := cmp.Diff("[web]\nweb01\n", content); diff != "" {
		t.Errorf("content mismatch (-want +got):\n%s", diff)
	}
}

// TestHostVariableValuesAreRenderedFaithfully pins that a host variable arrives as the text an
// inventory should carry rather than through Go's default formatting. A large integer reformatted
// into scientific notation, or an object printed as Go's map form, is a value the play reads
// differently from the one AWX held.
func TestHostVariableValuesAreRenderedFaithfully(t *testing.T) {
	t.Parallel()
	const doc = `{"inventory": [{"name": "prod", "hosts": [{"name": "web01", "variables": {
      "port": 2222,
      "big": 90071992547409931,
      "ratio": 0.5,
      "flag": true,
      "nothing": null,
      "list": [1, "two"],
      "object": {"a": "b"},
      "spaced": "with space",
      "quoted": "say \"hi\""
    }}]}]}`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	line := strings.SplitN(plan.Inventories[0].Content, "\n", 2)[0]
	for _, want := range []string{
		"port=2222", "big=90071992547409931", "ratio=0.5", "flag=true", "nothing=",
		`list="[1,\"two\"]"`, `object="{\"a\":\"b\"}"`, `spaced="with space"`,
		`quoted="say \"hi\""`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("host line is missing %q:\n%s", want, line)
		}
	}
}

// TestHostVariablesAreWrittenInAStableOrder pins that the same export produces the same inventory
// text every time. Map iteration order is random in Go, so an unsorted renderer would make two
// imports of one file produce different content and a diff nobody caused.
func TestHostVariablesAreWrittenInAStableOrder(t *testing.T) {
	t.Parallel()
	const doc = `{"inventory": [{"name": "prod", "hosts": [{"name": "web01", "variables": {
      "zeta": 1, "alpha": 2, "mid": 3, "beta": 4, "omega": 5
    }}], "variables": {"z": 1, "a": 2, "m": 3}}]}`
	first, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	for range 20 {
		next, err := FromAWX([]byte(doc), importNow)
		if err != nil {
			t.Fatalf("FromAWX() error = %v", err)
		}
		if diff := cmp.Diff(first.Inventories[0].Content, next.Inventories[0].Content); diff != "" {
			t.Fatalf("two imports of one export produced different inventories (-first +next):\n%s",
				diff)
		}
	}
	line := strings.SplitN(first.Inventories[0].Content, "\n", 2)[0]
	if !strings.HasPrefix(line, "web01 alpha=2 beta=4 mid=3 omega=5 zeta=1") {
		t.Errorf("host variables are not in sorted order: %s", line)
	}
}

// TestJSONScalarStringFallsBackWhenAValueCannotBeEncoded pins the last branch of the renderer. A
// value JSON cannot encode still has to produce text rather than an empty variable, since an
// inventory line with a bare key= reads as an empty value rather than as a failure.
func TestJSONScalarStringFallsBackWhenAValueCannotBeEncoded(t *testing.T) {
	t.Parallel()
	if got := jsonScalarString(make(chan int)); got == "" {
		t.Error("jsonScalarString() on an unencodable value = empty, want a printed form")
	}
	if got := jsonScalarString(json.RawMessage(`{"a":1}`)); got != `{"a":1}` {
		t.Errorf("jsonScalarString(RawMessage) = %q, want the raw JSON", got)
	}
}

// TestMapCredentialKindReadsTheCloudTypesByTheirSubstring pins the fuzzy half of the type table.
// AWX names a cloud credential type in prose, so the mapping is by substring, and a type mapped to
// the env fallback has to say it was inexact so an operator checks it.
func TestMapCredentialKindReadsTheCloudTypesByTheirSubstring(t *testing.T) {
	t.Parallel()
	tests := []struct {
		AWXType   string
		WantKind  credential.Kind
		WantExact bool
	}{
		{AWXType: "amazon web services", WantKind: credential.KindAWS, WantExact: true}, // Test 0.
		{AWXType: "AWS", WantKind: credential.KindAWS, WantExact: true},                 // Test 1.
		{AWXType: "Microsoft Azure Classic", WantKind: credential.KindAzure,
			WantExact: true}, // Test 2.
		{AWXType: "Google Compute Engine", WantKind: credential.KindGCP, WantExact: true}, // Test 3.
		{AWXType: "gce", WantKind: credential.KindGCP, WantExact: true},                   // Test 4.
		{AWXType: "GCP service account", WantKind: credential.KindGCP, WantExact: true},   // Test 5.
		{AWXType: "VMware vCenter", WantKind: credential.KindVMware, WantExact: true},     // Test 6.
		{AWXType: "vcenter", WantKind: credential.KindVMware, WantExact: true},            // Test 7.
		{AWXType: "Personal Access Token", WantKind: credential.KindToken,
			WantExact: true}, // Test 8.
		{AWXType: "bearer auth", WantKind: credential.KindToken, WantExact: true},   // Test 9.
		{AWXType: "SCM", WantKind: credential.KindSSHKey, WantExact: true},          // Test 10.
		{AWXType: "network", WantKind: credential.KindNetwork, WantExact: true},     // Test 11.
		{AWXType: "registry", WantKind: credential.KindRegistry, WantExact: true},   // Test 12.
		{AWXType: "", WantKind: credential.KindEnv, WantExact: false},               // Test 13.
		{AWXType: "Ansible Galaxy", WantKind: credential.KindEnv, WantExact: false}, // Test 14.
		{AWXType: "OpenStack", WantKind: credential.KindEnv, WantExact: false},      // Test 15.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.AWXType), func(t *testing.T) {
			t.Parallel()
			gotKind, gotExact := mapCredentialKind(test.AWXType, nil)
			if gotKind != test.WantKind || gotExact != test.WantExact {
				t.Errorf("mapCredentialKind(%q) = %q, %v, want %q, %v",
					test.AWXType, gotKind, gotExact, test.WantKind, test.WantExact)
			}
		})
	}
}

// TestJenkinsBundleAcceptsALoneConfigFileAndAZipOnDisk pins the two non-directory paths the walker
// takes. Zipping the jobs directory is the usual way to move a Jenkins off its own machine, so an
// archive handed over by path is read as one rather than wrapped as though it were a job.
func TestJenkinsBundleAcceptsALoneConfigFileAndAZipOnDisk(t *testing.T) {
	t.Parallel()
	t.Run("test 0 lone config file", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "nightly-backup")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("make fixture: %v", err)
		}
		path := filepath.Join(dir, "config.xml")
		if err := os.WriteFile(path, []byte("<project/>"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		bundle, err := JenkinsBundle(path)
		if err != nil {
			t.Fatalf("JenkinsBundle() error = %v", err)
		}
		if diff := cmp.Diff([]string{"nightly-backup"}, JenkinsJobNames(bundle),
			cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("job names mismatch (-want +got):\n%s", diff)
		}
	})
	t.Run("test 1 zip on disk", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "jobs.zip")
		archive := buildZip(t, map[string]string{"jobs/build/config.xml": "<project/>"})
		if err := os.WriteFile(path, archive, 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		bundle, err := JenkinsBundle(path)
		if err != nil {
			t.Fatalf("JenkinsBundle() error = %v", err)
		}
		if diff := cmp.Diff([]string{"build"}, JenkinsJobNames(bundle),
			cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("job names mismatch (-want +got):\n%s", diff)
		}
	})
	t.Run("test 2 jenkins home is entered for the caller", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		jobDir := filepath.Join(home, "jobs", "build")
		if err := os.MkdirAll(jobDir, 0o755); err != nil {
			t.Fatalf("make fixture: %v", err)
		}
		if err := os.WriteFile(filepath.Join(jobDir, "config.xml"),
			[]byte("<project/>"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		// The controller's own config.xml sits at the top of a JENKINS_HOME and is not a job.
		if err := os.WriteFile(filepath.Join(home, "config.xml"),
			[]byte("<hudson/>"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		bundle, err := JenkinsBundle(home)
		if err != nil {
			t.Fatalf("JenkinsBundle() error = %v", err)
		}
		if diff := cmp.Diff([]string{"build"}, JenkinsJobNames(bundle),
			cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("job names mismatch (-want +got):\n%s", diff)
		}
	})
	t.Run("test 3 a job's builds directory is never walked", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		jobDir := filepath.Join(home, "jobs", "build")
		buildDir := filepath.Join(jobDir, "builds", "1")
		if err := os.MkdirAll(buildDir, 0o755); err != nil {
			t.Fatalf("make fixture: %v", err)
		}
		if err := os.WriteFile(filepath.Join(jobDir, "config.xml"),
			[]byte("<project/>"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		if err := os.WriteFile(filepath.Join(buildDir, "config.xml"),
			[]byte("<project/>"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		bundle, err := JenkinsBundle(home)
		if err != nil {
			t.Fatalf("JenkinsBundle() error = %v", err)
		}
		if diff := cmp.Diff([]string{"build"}, JenkinsJobNames(bundle),
			cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("job names mismatch (-want +got):\n%s", diff)
		}
	})
}

// TestRootIsSequenceTellsTheTwoRundeckShapesApart pins the check that decides which parse error an
// operator is shown. Falling through to the wrapped attempt reported that the top level was the
// wrong shape when the file was fine and one scalar inside it was quoted.
func TestRootIsSequenceTellsTheTwoRundeckShapesApart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		Doc    string
		WantIs bool
	}{
		{Name: "bare list", Doc: "- name: a\n", WantIs: true},         // Test 0.
		{Name: "json list", Doc: `[{"name": "a"}]`, WantIs: true},     // Test 1.
		{Name: "mapping", Doc: "jobs:\n  - name: a\n", WantIs: false}, // Test 2.
		{Name: "scalar", Doc: "just a string", WantIs: false},         // Test 3.
		{Name: "empty", Doc: "", WantIs: false},                       // Test 4.
		{Name: "unparseable", Doc: "a: [1, 2", WantIs: false},         // Test 5.
		{Name: "flow list", Doc: "[a, b]", WantIs: true},              // Test 6.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := rootIsSequence([]byte(test.Doc)); got != test.WantIs {
				t.Errorf("rootIsSequence(%q) = %v, want %v", test.Doc, got, test.WantIs)
			}
		})
	}
}
