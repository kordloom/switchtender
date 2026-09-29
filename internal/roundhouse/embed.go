package roundhouse

import _ "embed"

// callbackPlugin is the SwitchTender Ansible callback plugin source, materialized to a temp directory
// at run time so ansible-playbook can load it and emit structured events.
//
//go:embed plugins/switchtender.py
var callbackPlugin string
