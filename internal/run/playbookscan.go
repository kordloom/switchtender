package run

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// taskKeywords are the Ansible task directives that are not the module being run. Anything else in
// a task mapping is the module, which is how Ansible itself reads a task.
var taskKeywords = map[string]bool{
	"name": true, "when": true, "loop": true, "with_items": true, "with_dict": true,
	"with_fileglob": true, "register": true, "tags": true, "become": true, "become_user": true,
	"become_method": true, "vars": true, "notify": true, "delegate_to": true, "run_once": true,
	"ignore_errors": true, "changed_when": true, "failed_when": true, "check_mode": true,
	"environment": true, "no_log": true, "until": true, "retries": true, "delay": true,
	"args": true, "loop_control": true, "any_errors_fatal": true, "throttle": true,
	"block": true, "rescue": true, "always": true, "listen": true, "connection": true,
	"local_action": true, "timeout": true, "poll": true, "async": true, "diff": true,
}

// destructiveModules are modules whose effect this product cannot undo, whatever arguments they
// carry. Matched against the last element of the module name, so the collection prefix does not
// have to be listed.
var destructiveModules = map[string]bool{
	"filesystem": true, "parted": true, "lvol": true, "lvg": true,
}

// dataBearingModules are modules whose removal destroys data rather than a definition, so state
// absent on one of them is permanent. A package or a service removed with state absent is a
// reinstall; a database or a filesystem path removed is gone.
var dataBearingModules = map[string]bool{
	"file": true, "mysql_db": true, "postgresql_db": true, "mongodb_db": true,
	"mssql_db": true, "influxdb_database": true, "elasticsearch_index": true,
	"s3_bucket": true, "gcs_bucket": true, "rds_instance": true, "ec2_instance": true,
	"lvol": true, "lvg": true, "user": true, "aws_s3": true,
}

// PlaybookSignals is what a playbook scan could see, and what it could not.
type PlaybookSignals struct {
	// Permanent lists the reasons the playbook holds something that cannot be undone.
	Permanent []string
	// Deferred reports that the playbook pulls in tasks this scan does not follow: roles,
	// include_tasks, import_playbook. It is carried so the grade can say its own scope rather than
	// implying it read everything that will run.
	Deferred bool
}

// ScanPlaybook reads an Ansible playbook's own text and reports what it can see about permanence.
//
// This exists because reversibility otherwise reads the command, and an Ansible run has none: the
// work is inside a file, so a playbook that drops every database graded exactly like one that
// restarts a service. Reading the file is what closes that gap.
//
// The scan is deliberately honest about its reach. It walks the plays in this file and their tasks,
// including blocks, and it does not follow roles, includes, or imported playbooks. Anything it
// misses leaves the grade where it was rather than lowering it, so an unread include can never make
// a run look safer than it is: the only direction this moves a grade is toward permanent.
func ScanPlaybook(content []byte) (PlaybookSignals, error) {
	var plays []map[string]any
	if err := yaml.Unmarshal(content, &plays); err != nil {
		return PlaybookSignals{}, fmt.Errorf("parse playbook: %w", err)
	}
	var out PlaybookSignals
	for _, play := range plays {
		if _, ok := play["roles"]; ok {
			out.Deferred = true
		}
		if _, ok := play["import_playbook"]; ok {
			out.Deferred = true
		}
		for _, key := range []string{"tasks", "pre_tasks", "post_tasks", "handlers"} {
			scanTaskList(play[key], &out)
		}
	}
	return out, nil
}

// scanTaskList walks a list of tasks, descending into blocks.
func scanTaskList(raw any, out *PlaybookSignals) {
	list, ok := raw.([]any)
	if !ok {
		return
	}
	for _, item := range list {
		task, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// A block holds more tasks, and its rescue and always paths run real work too.
		for _, nested := range []string{"block", "rescue", "always"} {
			if inner, ok := task[nested]; ok {
				scanTaskList(inner, out)
			}
		}
		scanTask(task, out)
	}
}

// scanTask grades one task mapping.
func scanTask(task map[string]any, out *PlaybookSignals) {
	for key, value := range task {
		if taskKeywords[key] {
			continue
		}
		module := key
		if i := strings.LastIndex(module, "."); i >= 0 {
			module = module[i+1:]
		}
		switch module {
		case "include_tasks", "import_tasks", "include_role", "import_role", "include":
			// Work this scan cannot see. Recorded so the grade can say so.
			out.Deferred = true
			continue
		case "command", "shell", "raw", "script":
			// The module's own text is a command, so the command markers apply to it.
			if text := rawParams(value); text != "" {
				for _, marker := range permanentMarkers {
					if strings.Contains(strings.ToLower(text), marker) {
						out.Permanent = append(out.Permanent,
							"a "+module+" task runs "+marker+", which cannot be undone from here")
					}
				}
			}
			continue
		}
		if destructiveModules[module] {
			out.Permanent = append(out.Permanent,
				"a task uses the "+module+" module, whose effect cannot be undone from here")
			continue
		}
		if dataBearingModules[module] && argState(value) == "absent" {
			out.Permanent = append(out.Permanent,
				"a task removes a "+module+" with state absent, which destroys what it held")
		}
	}
}

// rawParams returns a command module's text whether it was written inline or as a mapping.
func rawParams(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case map[string]any:
		if raw, ok := v["cmd"].(string); ok {
			return raw
		}
		if raw, ok := v["_raw_params"].(string); ok {
			return raw
		}
	}
	return ""
}

// argState returns a module's state argument, empty when it has none.
func argState(value any) string {
	args, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	state, _ := args["state"].(string)
	return strings.ToLower(state)
}
