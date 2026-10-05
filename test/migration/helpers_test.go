package migration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// runRecord is the part of a run the scenarios read.
type runRecord struct {
	// ID is the run's id.
	ID string `json:"id"`
	// Status is the run's lifecycle state.
	Status string `json:"status"`
	// Raw is the whole record as the API returned it.
	Raw map[string]any `json:"-"`
}

// lookup returns the id of the object of kind, a list path's collection name, called name.
func (in *install) lookup(s *server, kind, name string) string {
	in.t.Helper()
	if id := in.ids[kind][name]; id != "" {
		return id
	}
	r := in.must(s, "admin", "GET", "/v1/"+kind, nil, 200)
	var page map[string]json.RawMessage
	r.decode(in.t, &page)
	var items []struct {
		// ID is the object's id.
		ID string `json:"id"`
		// Name is the object's name.
		Name string `json:"name"`
	}
	if err := json.Unmarshal(page[kind], &items); err != nil {
		in.t.Fatalf("decode the %s list: %v", kind, err)
	}
	if in.ids[kind] == nil {
		in.ids[kind] = map[string]string{}
	}
	for _, it := range items {
		in.ids[kind][it.Name] = it.ID
	}
	id := in.ids[kind][name]
	if id == "" {
		in.t.Fatalf("no %s named %q after the import", kind, name)
	}
	return id
}

// template returns the id of the imported template called name.
func (in *install) template(s *server, name string) string {
	in.t.Helper()
	return in.lookup(s, "templates", name)
}

// launch launches a template as actor and returns the answer.
func (in *install) launch(s *server, actor, templateName string, body map[string]any) response {
	in.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	return in.api(s, actor, "POST", "/v1/templates/"+in.template(s, templateName)+"/launch", body)
}

// launched launches a template as actor, requires the launch to be accepted, and returns the run.
func (in *install) launched(s *server, actor, templateName string, body map[string]any) runRecord {
	in.t.Helper()
	r := in.launch(s, actor, templateName, body)
	if r.Status != 202 && r.Status != 201 {
		in.t.Fatalf("launch %q as %s = %d: %s", templateName, actor, r.Status, r.Body)
	}
	return decodeRun(in.t, r.Body)
}

// decodeRun reads a run record.
func decodeRun(t *testing.T, body []byte) runRecord {
	t.Helper()
	var rec runRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("decode the run: %v: %s", err, body)
	}
	if err := json.Unmarshal(body, &rec.Raw); err != nil {
		t.Fatalf("decode the run: %v: %s", err, body)
	}
	if rec.ID == "" {
		t.Fatalf("the run has no id: %s", body)
	}
	return rec
}

// getRun reads a run.
func (in *install) getRun(s *server, id string) runRecord {
	in.t.Helper()
	return decodeRun(in.t, in.must(s, "admin", "GET", "/v1/runs/"+id, nil, 200).Body)
}

// terminal reports whether a status is final.
func terminal(status string) bool {
	switch status {
	case "succeeded", "failed", "canceled", "interrupted", "rejected":
		return true
	}
	return false
}

// waitStatus waits until the run reaches one of the statuses and returns it.
func (in *install) waitStatus(s *server, id string, want ...string) runRecord {
	in.t.Helper()
	deadline := time.Now().Add(waitLimit)
	var last runRecord
	for time.Now().Before(deadline) {
		last = in.getRun(s, id)
		for _, w := range want {
			if last.Status == w {
				return last
			}
		}
		if terminal(last.Status) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs := in.api(s, "admin", "GET", "/v1/runs/"+id+"/logs", nil)
	in.t.Fatalf("run %s is %s, want %v\nrun: %v\nlog: %s", id, last.Status, want, last.Raw, logs.Body)
	return last
}

// waitDone waits until the run is final and returns it.
func (in *install) waitDone(s *server, id string) runRecord {
	in.t.Helper()
	return in.waitStatus(s, id, "succeeded", "failed", "canceled", "interrupted", "rejected")
}

// marked returns the hosts a marker name was written for, sorted.
func (in *install) marked(name string) []string {
	in.t.Helper()
	entries, err := os.ReadDir(in.markers)
	if err != nil {
		in.t.Fatalf("read the markers: %v", err)
	}
	var hosts []string
	for _, e := range entries {
		if host, ok := strings.CutPrefix(e.Name(), name+"-"); ok {
			hosts = append(hosts, host)
		}
	}
	sort.Strings(hosts)
	return hosts
}

// marker reads the marker one host wrote.
func (in *install) marker(name, host string) string {
	in.t.Helper()
	raw, err := os.ReadFile(filepath.Join(in.markers, name+"-"+host))
	if err != nil {
		in.t.Fatalf("read the %s marker for %s: %v", name, host, err)
	}
	return string(raw)
}

// waitMarker waits until a host's marker exists and returns its content.
func (in *install) waitMarker(name, host string) string {
	in.t.Helper()
	path := filepath.Join(in.markers, name+"-"+host)
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil && strings.HasSuffix(string(raw), "\n") {
			return string(raw)
		}
		time.Sleep(100 * time.Millisecond)
	}
	in.t.Fatalf("the %s marker for %s never appeared", name, host)
	return ""
}

// clearMarkers removes every marker, so the next run's hosts are read alone.
func (in *install) clearMarkers() {
	in.t.Helper()
	entries, err := os.ReadDir(in.markers)
	if err != nil {
		in.t.Fatalf("read the markers: %v", err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(in.markers, e.Name())); err != nil {
			in.t.Fatalf("remove marker %s: %v", e.Name(), err)
		}
	}
}

// fillCredential enters the field values an imported custom-type credential is waiting for. The
// secret value is recorded as one no record may hold.
func (in *install) fillCredential(s *server, name string, fields map[string]string) {
	in.t.Helper()
	id := in.lookup(s, "credentials", name)
	for k, v := range fields {
		if k == "token" {
			in.addSecret(v)
		}
	}
	in.must(s, "admin", "PUT", "/v1/credentials/"+id, map[string]any{"name": name, "fields": fields},
		200)
}

// runFileDirs lists the run directories under the dispatcher's root, which a finished run must have
// removed.
func (in *install) runFileDirs() []string {
	in.t.Helper()
	entries, err := os.ReadDir(in.runFiles)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		in.t.Fatalf("read the run directory root: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "run-") {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

// requireGone fails the scenario when any of the paths still exists.
func requireGone(t *testing.T, what string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if p == "" {
			t.Fatalf("%s: the play recorded no path, so nothing proves it was removed", what)
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s: %s still exists after the run ended (stat error %v)", what, p, err)
		}
	}
}

// field reads a nested field of a decoded record by dotted path, empty when absent.
func field(m map[string]any, path string) any {
	var cur any = m
	for _, part := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[part]
	}
	return cur
}

// str renders a decoded value for comparison and failure messages.
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		raw, _ := json.Marshal(x)
		return string(raw)
	}
}

// describe renders a value as indented JSON for a failure message.
func describe(v any) string {
	raw, _ := json.MarshalIndent(v, "", "  ")
	return string(raw)
}
