package roundhouse

import _ "embed"

// callbackPlugin is the Yardmaster Ansible callback plugin source, materialized to a temp directory
// at run time so ansible-playbook can load it and emit structured events.
//
//go:embed plugins/yardmaster.py
var callbackPlugin string
