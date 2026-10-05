package credential

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// kubeType is a custom type in the shape AWX documents for a kubeconfig: one multiline secret field
// written to a single file whose path reaches the run as KUBECONFIG and as an extra var.
func kubeType() CredentialType {
	return CredentialType{
		Name:              "Kubeconfig",
		Fields:            []Field{{Name: "kubeconfig", Secret: true, Multiline: true}},
		FileInjectors:     map[string]string{"template": "{{ kubeconfig }}"},
		EnvInjectors:      map[string]string{"KUBECONFIG": "{{ tower.filename }}"},
		ExtraVarInjectors: map[string]string{"k8s_kubeconfig": "{{awx.filename}}"},
	}
}

// TestFileInjectorValidate covers the rules a type with file injectors must satisfy.
func TestFileInjectorValidate(t *testing.T) {
	t.Parallel()
	named := func(files map[string]string, env map[string]string) CredentialType {
		return CredentialType{
			Name:          "Certs",
			Fields:        []Field{{Name: "cert"}, {Name: "key", Secret: true}},
			FileInjectors: files, EnvInjectors: env,
		}
	}
	tests := []struct {
		Type CredentialType
		Want error
	}{{ // Test 0: A single file referenced through tower.filename validates.
		Type: kubeType(), Want: nil,
	}, { // Test 1: Several named files, each referenced by name, validate.
		Type: named(map[string]string{"template.cert": "{{cert}}", "template.key": "{{key}}"},
			map[string]string{"CERT": "{{tower.filename.cert}}", "KEY": "{{ awx.filename.key }}"}),
		Want: nil,
	}, { // Test 2: A file whose path no injector references is refused.
		Type: named(map[string]string{"template.cert": "{{cert}}", "template.key": "{{key}}"},
			map[string]string{"CERT": "{{tower.filename.cert}}"}),
		Want: ErrBadType,
	}, { // Test 3: Mixing the single and the named spelling is refused.
		Type: named(map[string]string{"template": "{{cert}}", "template.key": "{{key}}"},
			map[string]string{"CERT": "{{tower.filename}}", "KEY": "{{tower.filename.key}}"}),
		Want: ErrBadType,
	}, { // Test 4: A file key that is not template or template.<name> is refused.
		Type: named(map[string]string{"cert": "{{cert}}"},
			map[string]string{"CERT": "{{tower.filename}}"}),
		Want: ErrBadType,
	}, { // Test 5: A reference to a file the type does not write is refused.
		Type: named(map[string]string{"template.cert": "{{cert}}"},
			map[string]string{"CERT": "{{tower.filename.cert}}", "KEY": "{{tower.filename.key}}"}),
		Want: ErrBadType,
	}, { // Test 6: The single-file spelling when the type writes named files is refused.
		Type: named(map[string]string{"template.cert": "{{cert}}"},
			map[string]string{"CERT": "{{tower.filename}}"}),
		Want: ErrBadType,
	}, { // Test 7: A file template referencing an undeclared field is refused.
		Type: named(map[string]string{"template": "{{nope}}"},
			map[string]string{"CERT": "{{tower.filename}}"}),
		Want: ErrBadType,
	}, { // Test 8: A file template referencing a file path is refused.
		Type: named(map[string]string{"template": "{{tower.filename}}"},
			map[string]string{"CERT": "{{tower.filename}}"}),
		Want: ErrBadType,
	}, { // Test 9: A dotted reference outside the filename namespace is refused.
		Type: named(map[string]string{"template": "{{cert}}"},
			map[string]string{"CERT": "{{tower.filename}}", "X": "{{tower.username}}"}),
		Want: ErrBadType,
	}, { // Test 10: A file-only type that hands its path over through extra vars validates.
		Type: CredentialType{
			Name: "Cfg", Fields: []Field{{Name: "body"}},
			FileInjectors:     map[string]string{"template": "[default]\n{{body}}\n"},
			ExtraVarInjectors: map[string]string{"cfg_path": "{{tower.filename}}"},
		},
		Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := test.Type.Validate()
			if test.Want == nil && err != nil {
				t.Fatalf("Validate() rejected a valid type: %v", err)
			}
			if !errors.Is(err, test.Want) {
				t.Errorf("Validate() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestRenderFilesAndInjectPaths proves the two halves of a file injector: the file's contents are
// rendered from the fields, line breaks and all, and the written path reaches every injector that
// references it.
func TestRenderFilesAndInjectPaths(t *testing.T) {
	t.Parallel()
	kubeconfig := "apiVersion: v1\nkind: Config\nusers:\n- user:\n    token: kube-s3cret\n"
	tests := []struct {
		Type          CredentialType
		Values        map[string]string
		Paths         map[string]string
		WantFiles     map[string]string
		WantEnv       []string
		WantExtraVars map[string]string
		Want          error
	}{{ // Test 0: A single kubeconfig file, its path in env and extra vars.
		Type:      kubeType(),
		Values:    map[string]string{"kubeconfig": kubeconfig},
		Paths:     map[string]string{SingleFile: "/run/a/cred-1"},
		WantFiles: map[string]string{SingleFile: kubeconfig},
		WantEnv:   []string{"KUBECONFIG=/run/a/cred-1"},
		WantExtraVars: map[string]string{
			"k8s_kubeconfig": "/run/a/cred-1",
		},
	}, { // Test 1: Two named files spliced into literal text, each path to its own variable.
		Type: CredentialType{
			Name:   "Client cert",
			Fields: []Field{{Name: "cert"}, {Name: "key", Secret: true}, {Name: "host"}},
			FileInjectors: map[string]string{
				"template.cert": "{{cert}}", "template.key": "# key\n{{ key }}",
			},
			EnvInjectors: map[string]string{
				"TLS_CERT": "{{tower.filename.cert}}",
				"TLS_ARGS": "--cert={{tower.filename.cert}} --key={{tower.filename.key}} {{host}}",
			},
		},
		Values: map[string]string{"cert": "CERT\nBODY", "key": "KEY\nBODY", "host": "h1"},
		Paths:  map[string]string{"cert": "/d/c", "key": "/d/k"},
		WantFiles: map[string]string{
			"cert": "CERT\nBODY", "key": "# key\nKEY\nBODY",
		},
		WantEnv: []string{"TLS_ARGS=--cert=/d/c --key=/d/k h1", "TLS_CERT=/d/c"},
	}, { // Test 2: A file that was never written is refused rather than injected as an empty path.
		Type:      kubeType(),
		Values:    map[string]string{"kubeconfig": kubeconfig},
		Paths:     nil,
		WantFiles: map[string]string{SingleFile: kubeconfig},
		Want:      ErrBadType,
	}, { // Test 3: A multiline value in a field an env injector reads is still refused.
		Type: CredentialType{
			Name: "Mixed", Fields: []Field{{Name: "body"}},
			FileInjectors: map[string]string{"template": "{{body}}"},
			EnvInjectors:  map[string]string{"P": "{{tower.filename}}", "B": "{{body}}"},
		},
		Values:    map[string]string{"body": "a\nEVIL=1"},
		Paths:     map[string]string{SingleFile: "/d/f"},
		WantFiles: map[string]string{SingleFile: "a\nEVIL=1"},
		Want:      ErrBadType,
	}, { // Test 4: A field value that looks like a path reference is not expanded in a file.
		Type:      kubeType(),
		Values:    map[string]string{"kubeconfig": "{{tower.filename}}"},
		Paths:     map[string]string{SingleFile: "/d/f"},
		WantFiles: map[string]string{SingleFile: "{{tower.filename}}"},
		WantEnv:   []string{"KUBECONFIG=/d/f"},
		WantExtraVars: map[string]string{
			"k8s_kubeconfig": "/d/f",
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := test.Type.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			files, err := test.Type.RenderFiles(test.Values)
			if err != nil {
				t.Fatalf("RenderFiles() error = %v", err)
			}
			if diff := cmp.Diff(test.WantFiles, files, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("RenderFiles() mismatch (-want +got):\n%s", diff)
			}
			inj, err := test.Type.Inject(test.Values, test.Paths)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Inject() error = %v, want %v", err, test.Want)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(test.WantEnv, inj.Env, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Inject() env mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantExtraVars, inj.ExtraVars, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Inject() extra vars mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFileInjectorSecretsReturned pins that a secret field written only to a file is still returned
// for masking, so a tool that prints the file shows *** in place of the value.
func TestFileInjectorSecretsReturned(t *testing.T) {
	t.Parallel()
	typ := kubeType()
	value := "users:\n- user:\n    token: kube-s3cret\n"
	inj, err := typ.Inject(map[string]string{"kubeconfig": value},
		map[string]string{SingleFile: "/d/f"})
	if err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	if diff := cmp.Diff([]string{value}, inj.Secrets); diff != "" {
		t.Errorf("Inject() secrets mismatch (-want +got):\n%s", diff)
	}
}

// certsType is a type writing two named files, cert and key, whose env injectors hand over the
// paths named in refs. origin is its Origin.
func certsType(origin string, refs ...string) *CredentialType {
	env := map[string]string{"HOST": "{{host}}"}
	for _, name := range refs {
		env[strings.ToUpper(name)+"_FILE"] = "{{ tower.filename." + name + " }}"
	}
	return &CredentialType{
		Name:   "Certs",
		Fields: []Field{{Name: "host"}, {Name: "cert"}, {Name: "key", Secret: true}},
		FileInjectors: map[string]string{
			"template.cert": "{{ cert }}", "template.key": "{{ key }}",
		},
		EnvInjectors: env, Origin: origin,
	}
}

// TestUnreferencedFilesByOrigin proves the rule for each origin: a file nothing references is
// refused for a type created here, kept for one imported from AWX, and named by UnreferencedFiles
// either way, so a run of an imported type can say which file nothing points at.
func TestUnreferencedFilesByOrigin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Type           *CredentialType
		WantUnrefed    []string
		WantErrPhrases []string
		Want           error
	}{{ // Test 0: Created here, every file referenced: valid, nothing to name.
		Type: certsType("", "cert", "key"),
	}, { // Test 1: Created here, key unreferenced: refused, naming key and its reference.
		Type: certsType("", "cert"), WantUnrefed: []string{"key"},
		WantErrPhrases: []string{
			"file key is written but no env or extra-var injector references its path",
			"{{ tower.filename.key }}", `"env": {"KEY_FILE": "{{ tower.filename.key }}"}`,
			"or delete the file injector",
		},
		Want: ErrBadType,
	}, { // Test 2: Created here, both unreferenced: refused, naming both.
		Type: certsType(""), WantUnrefed: []string{"cert", "key"},
		WantErrPhrases: []string{
			"files cert and key are written but no env or extra-var injector references their paths",
			"{{ tower.filename.cert }}, {{ tower.filename.key }}",
		},
		Want: ErrBadType,
	}, { // Test 3: Imported from AWX, key unreferenced: kept.
		Type: certsType(OriginAWX, "cert"), WantUnrefed: []string{"key"},
	}, { // Test 4: Imported from AWX, nothing referenced at all: kept.
		Type: certsType(OriginAWX), WantUnrefed: []string{"cert", "key"},
	}, { // Test 5: An origin this build does not know is held to the rule for types created here.
		Type: certsType("semaphore", "cert"), WantUnrefed: []string{"key"},
		WantErrPhrases: []string{"file key is written"}, Want: ErrBadType,
	}, { // Test 6: The single unnamed file, unreferenced and created here, gets the bare spelling.
		Type: &CredentialType{
			Name: "Token file", Fields: []Field{{Name: "token", Secret: true}},
			FileInjectors: map[string]string{"template": "token={{ token }}"},
			EnvInjectors:  map[string]string{"SERVICE": "billing"},
		},
		WantUnrefed: []string{SingleFile},
		WantErrPhrases: []string{
			"file template is written",
			`"env": {"CREDENTIAL_FILE": "{{ tower.filename }}"}`,
		},
		Want: ErrBadType,
	}, { // Test 7: A path handed over through an extra var, under the awx alias, counts.
		Type: &CredentialType{
			Name: "Cfg", Fields: []Field{{Name: "body"}},
			FileInjectors:     map[string]string{"template": "{{ body }}"},
			ExtraVarInjectors: map[string]string{"cfg": "--config={{ awx.filename }}"},
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantUnrefed, test.Type.UnreferencedFiles(),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("UnreferencedFiles() mismatch (-want +got):\n%s", diff)
			}
			err := test.Type.Validate()
			if !errors.Is(err, test.Want) || (test.Want == nil && err != nil) {
				t.Fatalf("Validate() error = %v, want %v", err, test.Want)
			}
			for _, phrase := range test.WantErrPhrases {
				if !strings.Contains(err.Error(), phrase) {
					t.Errorf("Validate() error = %q, want it to say %q", err, phrase)
				}
			}
		})
	}
}

// TestValidateEditKeepsOnlyWhatTheImportBrought proves an edit made here cannot add a file nothing
// references, even to an imported type, while the files the import brought that way stay allowed.
func TestValidateEditKeepsOnlyWhatTheImportBrought(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Prev       *CredentialType
		Next       *CredentialType
		WantPhrase string
		Want       error
	}{{ // Test 0: An imported type edited without touching its files keeps its unreferenced key.
		Prev: certsType(OriginAWX, "cert"), Next: certsType(OriginAWX, "cert"),
	}, { // Test 1: An edit that stops referencing cert adds an unreferenced file and is refused.
		Prev: certsType(OriginAWX, "cert"), Next: certsType(OriginAWX),
		WantPhrase: "file cert is written", Want: ErrBadType,
	}, { // Test 2: An edit that starts referencing key leaves nothing unreferenced and is valid.
		Prev: certsType(OriginAWX, "cert"), Next: certsType(OriginAWX, "cert", "key"),
	}, { // Test 3: A type created here gets nothing kept, so an unreferenced file is refused.
		Prev: certsType("", "cert", "key"), Next: certsType("", "cert"),
		WantPhrase: "file key is written", Want: ErrBadType,
	}, { // Test 4: With no previous version the edit is held to the rule for types created here.
		Prev: nil, Next: certsType(OriginAWX, "cert"),
		WantPhrase: "file key is written", Want: ErrBadType,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := test.Next.ValidateEdit(test.Prev)
			if !errors.Is(err, test.Want) || (test.Want == nil && err != nil) {
				t.Fatalf("ValidateEdit() error = %v, want %v", err, test.Want)
			}
			if err != nil && !strings.Contains(err.Error(), test.WantPhrase) {
				t.Errorf("ValidateEdit() error = %q, want it to say %q", err, test.WantPhrase)
			}
		})
	}
}
