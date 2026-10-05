package importer

import (
	"encoding/json"
	"fmt"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/util"
)

// credTypeNotes returns what a person should know about an imported custom type beyond its fields:
// each file it writes that no injector references, and whether it holds a kubeconfig the built-in
// kind could carry. Both are review items. The type came across either way, unchanged.
func credTypeNotes(typ *credential.CredentialType) []string {
	var notes []string
	for _, name := range typ.UnreferencedFiles() {
		notes = append(notes, unreferencedFileNote(name))
	}
	if field, ok := typ.KubeconfigShaped(); ok {
		notes = append(notes, kubeconfigNote(field))
	}
	return notes
}

// unreferencedFileNote explains a file an imported type writes and nothing points the tool at: that
// it came across as it was, what a run does with it, and the reference that fixes it.
func unreferencedFileNote(name string) string {
	return fmt.Sprintf("file %s is written for each run, but no env or extra-var injector "+
		"references its path, so nothing tells the tool where it is. The type came across as it "+
		"was defined in AWX, and each run of a credential of it carries a warning naming the file. "+
		"Hand the path over with %s in an env or extra-var injector, or delete the file injector. "+
		"A type defined in SwitchTender has to reference every file it writes", name,
		credential.FileRef(name))
}

// kubeconfigNote explains a type that holds a kubeconfig in one file: what masking it keeps as
// imported, what the built-in kubeconfig kind does instead, and the one request that switches a
// credential of it. The import switches nothing, since the switch changes what a run's output
// shows.
func kubeconfigNote(field string) string {
	return fmt.Sprintf("it writes the kubeconfig in field %q to a file, which the built-in "+
		"kubeconfig kind also does. As imported, its credentials mask every line of that document "+
		"wherever a tool prints it, ordinary lines such as \"kind: Config\" included. The "+
		"kubeconfig kind masks only the secrets inside the document, such as tokens, passwords, and "+
		"client keys, and points %s at the file. Switching a credential is one step when its value "+
		"is entered: PUT /v1/credentials/{id} with its name, \"kind\": \"kubeconfig\", and the "+
		"document as \"secret\", in place of a fields object. Nothing switches unless you do it",
		field, util.JoinWords(credential.KubeconfigEnvVars, "and"))
}

// noteKubeconfigSwitch gives a credential of a type shaped like a kubeconfig the exact request that
// moves it to the built-in kind as its value is entered. awxName is its name in the export and name
// its name here, which the body has to carry so the update does not rename it. A credential of any
// other type gets nothing.
func (p *Plan) noteKubeconfigSwitch(awxName, name string, typ *credential.CredentialType) {
	if _, ok := typ.KubeconfigShaped(); !ok {
		return
	}
	// A credential name is free text, so it is encoded as JSON rather than spliced in. Encoding a
	// string cannot fail.
	quoted, _ := json.Marshal(name)
	p.warn("credential %q can switch to the built-in kubeconfig kind in the same request that "+
		"enters its value, which masks only the secrets inside the document: PUT "+
		"/v1/credentials/{id} with {\"name\": %s, \"kind\": \"kubeconfig\", \"secret\": "+
		"\"<the kubeconfig document>\"}", awxName, quoted)
}
