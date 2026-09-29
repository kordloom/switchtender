package importer

import (
	"slices"
	"testing"

	"github.com/kordloom/switchtender/internal/credential"
)

// fidelityExport is an AWX export shaped the way awxkit writes one, carrying the settings the
// importer used to drop: a survey that is switched off, privilege escalation, a machine credential
// with a become password, and projects whose private repositories sync with a credential.
const fidelityExport = `{
  "projects": [
    {"name": "infra", "scm_type": "git", "scm_url": "git@github.com:acme/infra.git",
     "credential": {"name": "github-key", "credential_type": {"name": "Source Control", "kind": "scm"}}},
    {"name": "web", "scm_type": "git", "scm_url": "https://github.com/acme/web.git",
     "credential": {"name": "github-login", "credential_type": {"name": "Source Control", "kind": "scm"}}}
  ],
  "credentials": [
    {"name": "github-key", "credential_type": {"name": "Source Control", "kind": "scm"},
     "inputs": {"username": "git", "ssh_key_data": "$encrypted$"}},
    {"name": "github-login", "credential_type": {"name": "Source Control", "kind": "scm"},
     "inputs": {"username": "deploy", "password": "$encrypted$"}},
    {"name": "legacy", "credential_type": {"name": "Machine", "kind": "ssh"},
     "inputs": {"username": "ops", "password": "$encrypted$", "become_method": "su",
                "become_password": "$encrypted$"}}
  ],
  "job_templates": [
    {"name": "Survey off", "playbook": "site.yml", "project": {"name": "infra"},
     "survey_enabled": false,
     "survey_spec": {"spec": [{"variable": "window", "question_name": "Window", "type": "text",
                               "required": true}]}},
    {"name": "Survey on", "playbook": "site.yml", "project": {"name": "infra"},
     "survey_enabled": true,
     "survey_spec": {"spec": [{"variable": "window", "question_name": "Window", "type": "text",
                               "required": true}]}},
    {"name": "Escalates", "playbook": "site.yml", "project": {"name": "infra"},
     "become_enabled": true, "credentials": [{"name": "legacy"}]},
    {"name": "Says no itself", "playbook": "site.yml", "project": {"name": "infra"},
     "become_enabled": true, "extra_vars": "ansible_become: false"}
  ]
}`

// TestAWXImportCarriesWhatItUsedToDrop pins four AWX settings the importer lost. A survey switched off
// in AWX arrived active and required, so its template could not launch without answers. Privilege
// escalation was dropped, so the run printed become=UNSET. A machine credential's become password went
// unmentioned. A project's source control credential was never read, so a private repository failed
// its first sync.
func TestAWXImportCarriesWhatItUsedToDrop(t *testing.T) {
	t.Parallel()
	plan, err := FromAWX([]byte(fidelityExport), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	templates := map[string]int{}
	for i, tpl := range plan.Templates {
		templates[tpl.Name] = i
	}
	tpl := func(name string) int {
		t.Helper()
		i, ok := templates[name]
		if !ok {
			t.Fatalf("template %q was not imported", name)
		}
		return i
	}

	if s := plan.Templates[tpl("Survey off")].Survey; len(s) != 0 {
		t.Errorf("a survey switched off in AWX imported with %d questions, want none", len(s))
	}
	if s := plan.Templates[tpl("Survey on")].Survey; len(s) != 1 {
		t.Errorf("a survey switched on in AWX imported with %d questions, want 1", len(s))
	}

	if v := plan.Templates[tpl("Escalates")].ExtraVars["ansible_become"]; v != true {
		t.Errorf("a template that escalates in AWX carries ansible_become %v, want true", v)
	}
	if v := plan.Templates[tpl("Says no itself")].ExtraVars["ansible_become"]; v != false {
		t.Errorf("an ansible_become the template already sets was overridden to %v", v)
	}

	byName := map[string]*credential.Credential{}
	for _, c := range plan.Credentials {
		byName[c.Name] = c
	}
	become, ok := byName["legacy (become)"]
	if !ok || become.Kind != credential.KindBecomePassword {
		t.Fatalf("the become password of credential legacy did not arrive as its own shell: %v", byName)
	}
	attached := plan.Templates[tpl("Escalates")].CredentialIDs
	if !slices.Contains(attached, byName["legacy"].ID) || !slices.Contains(attached, become.ID) {
		t.Errorf("template Escalates attaches %v, want both legacy and its become password", attached)
	}

	for _, p := range plan.Projects {
		switch p.Name {
		case "infra":
			if p.CredentialID != byName["github-key"].ID {
				t.Errorf("project infra syncs with %q, want the key credential github-key", p.CredentialID)
			}
		case "web":
			if p.CredentialID != "" {
				t.Errorf("project web attached %q, a credential a project cannot sync with", p.CredentialID)
			}
		}
	}
	if _, ok := warningContaining(t, plan.Warnings, `project "web"`, `"github-login"`, "SSH"); !ok {
		t.Errorf("the project whose credential cannot be attached was not reported: %v", plan.Warnings)
	}
}
