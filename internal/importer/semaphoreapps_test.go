package importer

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/run"
)

// semaphoreAppsExport is a Semaphore backup with a template for each tool Semaphore runs, and access
// keys attached everywhere Semaphore attaches them.
const semaphoreAppsExport = `{"meta": {"name": "ops"},
  "repositories": [
    {"name": "infra", "git_url": "git@example.test:acme/infra.git", "ssh_key": "deploy-ssh"},
    {"name": "web", "git_url": "https://example.test/acme/web.git", "ssh_key": "git-login"}],
  "inventories": [
    {"name": "prod-fleet", "type": "static", "inventory": "[web]\nweb01\n",
     "ssh_key": "ops-login", "become_key": "sudo-pass"},
    {"name": "tf-ws", "type": "terraform-workspace", "inventory": "prod", "ssh_key": "None"}],
  "keys": [
    {"name": "None", "type": "none"},
    {"name": "deploy-ssh", "type": "ssh"},
    {"name": "git-login", "type": "login_password"},
    {"name": "ops-login", "type": "login_password"},
    {"name": "sudo-pass", "type": "login_password"},
    {"name": "vault-pass", "type": "login_password"}],
  "templates": [
    {"name": "Deploy", "app": "ansible", "playbook": "site.yml", "repository": "infra",
     "inventory": "prod-fleet", "vaults": [{"vault_key": "vault-pass", "type": "password", "name": "prod"}]},
    {"name": "Backup", "app": "bash", "playbook": "scripts/backup.sh", "repository": "infra",
     "inventory": null, "arguments": "[\"--full\"]"},
    {"name": "Plan network", "app": "terraform", "playbook": "terraform/network",
     "repository": "infra", "inventory": "tf-ws"},
    {"name": "Report", "app": "python", "playbook": "scripts/report.py", "repository": "infra",
     "arguments": "[\"weekly\"]"},
    {"name": "Rollout", "app": "pulumi", "playbook": "stacks/web", "repository": "infra"}]}`

// TestASemaphoreBackupRunsEachTemplateWithItsOwnTool pins the tool on a Semaphore template and the
// places its access keys are attached. The tool was ignored, so a Bash script and a Terraform directory
// imported as Ansible templates whose playbook was the script or the directory, and neither could run.
// The keys were imported and attached to nothing, so a template that reached its hosts, cloned its
// repository, or unlocked its vault with one did none of those here.
func TestASemaphoreBackupRunsEachTemplateWithItsOwnTool(t *testing.T) {
	t.Parallel()
	plan, err := FromSemaphore([]byte(semaphoreAppsExport), importNow)
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}
	tpl := map[string]int{}
	for i, x := range plan.Templates {
		tpl[x.Name] = i
	}
	cred := map[string]*credential.Credential{}
	for _, c := range plan.Credentials {
		cred[c.Name] = c
	}

	if _, ok := tpl["Rollout"]; ok {
		t.Error("a pulumi template imported, and there is no pulumi here to run it")
	}
	backup := plan.Templates[tpl["Backup"]]
	if backup.Tool != run.ToolBash || backup.Command != "bash scripts/backup.sh --full" || backup.Playbook != "" {
		t.Errorf("Backup = tool %q command %q playbook %q, want bash running the script", backup.Tool,
			backup.Command, backup.Playbook)
	}
	network := plan.Templates[tpl["Plan network"]]
	if network.Tool != run.ToolTerraform || network.Command != "terraform/network" || !network.DryRun ||
		network.InventoryID != "" {
		t.Errorf("Plan network = tool %q command %q dry run %v inventory %q, want a terraform plan of "+
			"the directory with no inventory", network.Tool, network.Command, network.DryRun,
			network.InventoryID)
	}
	if report := plan.Templates[tpl["Report"]]; report.Tool != run.ToolPython {
		t.Errorf("Report tool = %q, want python", report.Tool)
	}

	if _, ok := cred["None"]; ok {
		t.Error("the type none key imported as a credential, asking for a secret it never held")
	}
	deploy := plan.Templates[tpl["Deploy"]]
	vault := cred["vault-pass"]
	if vault == nil || vault.Kind != credential.KindVaultPassword || vault.VaultID != "prod" ||
		!slices.Contains(deploy.CredentialIDs, vault.ID) {
		t.Errorf("the vault key did not arrive as the vault password Deploy unlocks: %+v", vault)
	}
	var fleet []string
	for _, inv := range plan.Inventories {
		if inv.Name == "prod-fleet" {
			fleet = inv.CredentialIDs
		}
	}
	if cred["ops-login"].Kind != credential.KindSSHPassword || !slices.Contains(fleet, cred["ops-login"].ID) {
		t.Errorf("the inventory's login is not an SSH password on the inventory: %+v", cred["ops-login"])
	}
	if cred["sudo-pass"].Kind != credential.KindBecomePassword || !slices.Contains(fleet, cred["sudo-pass"].ID) {
		t.Errorf("the inventory's become key is not a become password on it: %+v", cred["sudo-pass"])
	}
	for _, p := range plan.Projects {
		switch {
		case strings.HasSuffix(p.Name, "infra") && p.CredentialID != cred["deploy-ssh"].ID:
			t.Errorf("project %q clones with %q, want the SSH key", p.Name, p.CredentialID)
		case strings.HasSuffix(p.Name, "web") && p.CredentialID != "":
			t.Errorf("project %q attached the HTTPS login %q, which a project here cannot use",
				p.Name, p.CredentialID)
		}
	}
}

// TestASemaphorePythonTemplateRunsItsFile runs the command a Semaphore Python template imports as, in
// a checkout holding the script, and checks the script ran with its arguments.
func TestASemaphorePythonTemplateRunsItsFile(t *testing.T) {
	t.Parallel()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "scripts"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	script := "import sys\nif __name__ == \"__main__\":\n    print(' '.join(sys.argv))\n"
	if err := os.WriteFile(filepath.Join(checkout, "scripts", "report.py"), []byte(script), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cmd := exec.Command(python, "-c", semaphoreScript(run.ToolPython, "scripts/report.py", []string{"weekly", "it's"}))
	cmd.Dir = checkout
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the imported command failed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "scripts/report.py weekly it's" {
		t.Errorf("the script saw %q, want its path and both arguments", got)
	}
}
