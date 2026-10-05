package migration

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claim is one entry a receipt discloses, with the bodies an outcome entry carries decoded.
type claim struct {
	// Actor names who acted.
	Actor string `json:"actor"`
	// ActorType classifies the actor.
	ActorType string `json:"actor_type"`
	// OnBehalfOf names whose authority the actor used.
	OnBehalfOf string `json:"on_behalf_of"`
	// Method is the HTTP method, or RUN, or CLI.
	Method string `json:"method"`
	// Path is the request or event path.
	Path string `json:"path"`
	// OutcomeBody is an outcome entry's disclosed outcome, as JSON text.
	OutcomeBody string `json:"outcome_body"`
	// SpecBody is an outcome entry's disclosed spec, as JSON text.
	SpecBody string `json:"spec_body"`
	// Outcome is OutcomeBody decoded.
	Outcome map[string]any `json:"-"`
	// Spec is SpecBody decoded.
	Spec map[string]any `json:"-"`
}

// receipt is a run's receipt, verified offline, with its claims decoded.
type receipt struct {
	// RunID is the run the receipt is for.
	RunID string
	// Raw is the signed receipt.
	Raw []byte
	// Claims are the entries it discloses, oldest first.
	Claims []claim
}

// outcome returns the receipt's outcome claim, failing the scenario when it has none.
func (r *receipt) outcome(t *testing.T) claim {
	t.Helper()
	for _, c := range r.Claims {
		if c.Method == "RUN" && strings.HasPrefix(c.Path, "/runs/"+r.RunID+"/outcome/") {
			return c
		}
	}
	t.Fatalf("the receipt discloses no outcome entry for %s: %s", r.RunID, r.Raw)
	return claim{}
}

// launch returns the entry that created the run, which a receipt starts from.
func (r *receipt) launch(t *testing.T) claim {
	t.Helper()
	if len(r.Claims) == 0 {
		t.Fatalf("the receipt for %s discloses nothing", r.RunID)
	}
	return r.Claims[0]
}

// find returns the receipt's claims whose path contains every fragment.
func (r *receipt) find(fragments ...string) []claim {
	var out []claim
	for _, c := range r.Claims {
		match := true
		for _, f := range fragments {
			if !strings.Contains(c.Path, f) {
				match = false
				break
			}
		}
		if match {
			out = append(out, c)
		}
	}
	return out
}

// auditEntry is one entry of the audit listing.
type auditEntry struct {
	// Actor names who acted.
	Actor string `json:"actor"`
	// ActorType classifies the actor.
	ActorType string `json:"actor_type"`
	// OnBehalfOf names whose authority the actor used.
	OnBehalfOf string `json:"on_behalf_of"`
	// Method is the HTTP method, or RUN, or CLI.
	Method string `json:"method"`
	// Path is the request or event path.
	Path string `json:"path"`
	// Seq is the entry's position in the chain.
	Seq int64 `json:"seq"`
}

// evidence is what a scenario's install recorded, exported and verified offline.
type evidence struct {
	// Bundle is the signed export of the whole chain.
	Bundle []byte
	// Receipts are the verified receipts of the scenario's runs, by run id.
	Receipts map[string]*receipt
	// Audit is the audit listing, newest first.
	Audit []auditEntry
}

// entries returns the audit entries whose path contains every fragment.
func (e *evidence) entries(fragments ...string) []auditEntry {
	var out []auditEntry
	for _, a := range e.Audit {
		match := true
		for _, f := range fragments {
			if !strings.Contains(a.Path, f) {
				match = false
				break
			}
		}
		if match {
			out = append(out, a)
		}
	}
	return out
}

// checkEvidence is the check every scenario ends with. It exports the whole chain and has the
// LoomSeal reference verifier check it offline, does the same for each run's receipt, reads every
// record the install keeps about the runs, and fails the scenario if any secret the install was
// given appears in any of them, in a server's log, in an API answer, or in the database itself.
func (in *install) checkEvidence(s *server, runIDs ...string) *evidence {
	in.t.Helper()
	// A run reads as finished from its terminal write and commits its outcome just after it, so the
	// chain is read once every run's outcome is on it.
	for _, id := range runIDs {
		in.awaitOutcome(s, id)
	}
	ev := &evidence{Receipts: map[string]*receipt{}}
	dir := filepath.Join(in.root, "evidence")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		in.t.Fatalf("create the evidence directory: %v", err)
	}

	bundle := in.must(s, "admin", "GET", "/v1/audit/bundle", nil, 200)
	ev.Bundle = bundle.Body
	bundlePath := filepath.Join(dir, "chain.loomseal.json")
	if err := os.WriteFile(bundlePath, bundle.Body, 0o600); err != nil {
		in.t.Fatalf("write the exported chain: %v", err)
	}
	if rep := verifyOffline(in.t, bundlePath); !rep.OK {
		in.t.Fatalf("LoomSeal refused the exported chain: %v", rep.Problems)
	}

	var listing struct {
		// Entries are the audit entries.
		Entries []auditEntry `json:"entries"`
	}
	in.must(s, "admin", "GET", "/v1/audit?limit=1000", nil, 200).decode(in.t, &listing)
	ev.Audit = listing.Entries

	for _, id := range runIDs {
		ev.Receipts[id] = in.verifiedReceipt(s, dir, id)
		for _, path := range []string{"", "/logs", "/events", "/evidence", "/steps"} {
			in.must(s, "admin", "GET", "/v1/runs/"+id+path, nil, 200)
		}
	}
	in.scanForSecrets()
	return ev
}

// awaitOutcome waits until the run's outcome is on the chain, which its receipt reports by
// existing. Only the answer for an outcome not yet committed is waited out.
func (in *install) awaitOutcome(s *server, id string) {
	in.t.Helper()
	path := "/v1/runs/" + id + "/receipt"
	deadline := time.Now().Add(waitLimit)
	for {
		r := in.api(s, "admin", "GET", path, nil)
		switch {
		case r.Status == http.StatusOK:
			return
		case r.Status != http.StatusConflict:
			in.t.Fatalf("GET %s as admin = %d, want 200 once the outcome is committed: %s", path,
				r.Status, r.Body)
		case time.Now().After(deadline):
			in.t.Fatalf("run %s never committed its outcome: %s", id, r.Body)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// verifiedReceipt fetches a run's receipt, has LoomSeal verify it offline, and decodes its claims.
func (in *install) verifiedReceipt(s *server, dir, id string) *receipt {
	in.t.Helper()
	raw := in.must(s, "admin", "GET", "/v1/runs/"+id+"/receipt", nil, 200).Body
	path := filepath.Join(dir, id+".receipt.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		in.t.Fatalf("write the receipt: %v", err)
	}
	if rep := verifyOffline(in.t, path); !rep.OK {
		in.t.Fatalf("LoomSeal refused the receipt of %s: %v", id, rep.Problems)
	}
	var doc struct {
		// Claims are the disclosed entries.
		Claims []struct {
			// Payload is the entry.
			Payload claim `json:"payload"`
		} `json:"claims"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		in.t.Fatalf("decode the receipt of %s: %v", id, err)
	}
	rec := &receipt{RunID: id, Raw: raw}
	for _, c := range doc.Claims {
		p := c.Payload
		if p.OutcomeBody != "" {
			if err := json.Unmarshal([]byte(p.OutcomeBody), &p.Outcome); err != nil {
				in.t.Fatalf("decode the outcome body of %s: %v", id, err)
			}
		}
		if p.SpecBody != "" {
			if err := json.Unmarshal([]byte(p.SpecBody), &p.Spec); err != nil {
				in.t.Fatalf("decode the spec body of %s: %v", id, err)
			}
		}
		rec.Claims = append(rec.Claims, p)
	}
	return rec
}

// scanForSecrets fails the scenario when any secret the install holds appears in an API answer, a
// server log, or the database. Every receipt, bundle, dossier, run record, log, and event read
// through the API is an API answer, so they are all covered.
func (in *install) scanForSecrets() {
	in.t.Helper()
	in.mu.Lock()
	secrets := append([]string(nil), in.secrets...)
	in.mu.Unlock()

	var places []answer
	if v, ok := answers.Load(in.root); ok {
		log := v.(*answerLog)
		log.mu.Lock()
		places = append(places, log.bodies...)
		log.mu.Unlock()
	}
	for _, s := range in.servers {
		raw, err := os.ReadFile(s.logPath)
		if err != nil {
			in.t.Fatalf("read the log of server %s: %v", s.name, err)
		}
		places = append(places, answer{What: "the log of server " + s.name, Body: raw})
	}
	places = append(places, in.databaseText()...)

	for _, secret := range secrets {
		if len(secret) < 8 {
			in.t.Fatalf("secret %q is too short to scan for without false matches", secret)
		}
		for _, p := range places {
			if bytes.Contains(p.Body, []byte(secret)) {
				in.t.Errorf("a secret (%d bytes, starting %q) appears in %s", len(secret),
					secret[:4], p.What)
			}
		}
	}
}

// databaseText returns the install's stored data as bytes to scan: the SQLite files themselves, or
// every row of every PostgreSQL table rendered as text.
func (in *install) databaseText() []answer {
	in.t.Helper()
	if !strings.Contains(in.db, "://") {
		var out []answer
		for _, suffix := range []string{"", "-wal", "-shm"} {
			raw, err := os.ReadFile(in.db + suffix)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				in.t.Fatalf("read the database file: %v", err)
			}
			out = append(out, answer{What: "the database file " + filepath.Base(in.db+suffix), Body: raw})
		}
		return out
	}
	db, err := sql.Open("pgx", in.db)
	if err != nil {
		in.t.Fatalf("open the scenario database: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	if err != nil {
		in.t.Fatalf("list the scenario tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			in.t.Fatalf("read a table name: %v", err)
		}
		tables = append(tables, name)
	}
	_ = rows.Close()
	var out []answer
	for _, table := range tables {
		var text sql.NullString
		q := fmt.Sprintf(`SELECT string_agg(t::text, E'\n') FROM %q t`, table)
		if err := db.QueryRow(q).Scan(&text); err != nil {
			in.t.Fatalf("read table %s: %v", table, err)
		}
		out = append(out, answer{What: "the database table " + table, Body: []byte(text.String)})
	}
	return out
}
