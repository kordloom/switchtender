package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// composeFile is the part of docker-compose.yml these checks read.
type composeFile struct {
	// Services are the file's services by name.
	Services map[string]composeService `yaml:"services"`
}

// composeService is one service's profiles, command, environment, and mounts.
type composeService struct {
	// Profiles select the service.
	Profiles []string `yaml:"profiles"`
	// Image names a prebuilt image, empty for a service built from the repository.
	Image string `yaml:"image"`
	// Command is the argument list the image runs.
	Command []string `yaml:"command"`
	// Environment maps variable names to values.
	Environment map[string]string `yaml:"environment"`
	// Volumes are the service's mounts, in either Compose syntax.
	Volumes []any `yaml:"volumes"`
}

// mountTargets returns where a service's volumes land and, for a bind, whether a missing source is
// refused rather than created.
func (s composeService) mountTargets() map[string]bool {
	out := map[string]bool{}
	for _, v := range s.Volumes {
		switch m := v.(type) {
		case string:
			parts := strings.Split(m, ":")
			if len(parts) >= 2 {
				out[parts[1]] = false
			}
		case map[string]any:
			target, _ := m["target"].(string)
			refused := false
			if bind, ok := m["bind"].(map[string]any); ok {
				if create, ok := bind["create_host_path"].(bool); ok && !create {
					refused = true
				}
			}
			out[target] = refused
		}
	}
	return out
}

// TestTheComposeStackRunsOnCommunity pins the documented Docker path to what Community can run.
//
// The stack profile was PostgreSQL, and creating a PostgreSQL schema is Team, so the quickstart's
// Docker command failed on every install without a license. The stack is one server on SQLite with
// its data in a volume. The team profile carries what processes sharing one database need: the
// signing key, since none of them mints one, and a license that is refused when missing rather than
// replaced by an empty directory.
func TestTheComposeStackRunsOnCommunity(t *testing.T) {
	t.Parallel()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	var file composeFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse docker-compose.yml: %v", err)
	}
	var stack, team []string
	for name, svc := range file.Services {
		if slices.Contains(svc.Profiles, "stack") {
			stack = append(stack, name)
		}
		if slices.Contains(svc.Profiles, "team") {
			team = append(team, name)
		}
	}
	if len(stack) == 0 || len(team) == 0 {
		t.Fatalf("stack profile %v, team profile %v, want both", stack, team)
	}
	for _, name := range stack {
		svc := file.Services[name]
		if strings.Contains(svc.Image, "postgres") || strings.Contains(strings.Join(svc.Command, " "), "postgres://") {
			t.Errorf("the stack profile's %s needs PostgreSQL, whose schema only a Team install creates", name)
		}
		if _, ok := svc.mountTargets()["/data"]; !ok && svc.Image == "" {
			t.Errorf("the stack profile's %s keeps its database and signing key in no volume", name)
		}
	}
	for _, name := range team {
		svc := file.Services[name]
		if svc.Image != "" {
			continue
		}
		if _, ok := svc.Environment["SWITCHTENDER_AUDIT_KEY"]; !ok {
			t.Errorf("the team profile's %s gets no signing key, so it signs nothing", name)
		}
		refused, ok := svc.mountTargets()["/data/switchtender-license.json"]
		if !ok || !refused {
			t.Errorf("the team profile's %s mounts no license, or makes an empty directory of a "+
				"missing one", name)
		}
	}
}
