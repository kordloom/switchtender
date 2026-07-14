package roundhouse

import _ "embed"

// callbackPlugin is the Railwarden Ansible callback plugin source, materialized to a temp directory
// at run time so ansible-playbook can load it and emit structured events.
//
//go:embed plugins/railwarden.py
var callbackPlugin string
