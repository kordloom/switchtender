package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// phaseInvariants holds the deployed install to the properties that must be true of every install,
// whatever it was configured to do.
//
// The other phases drive a story: launch this, hold that, approve it, verify the receipt. Each one
// checks the answer to the question it asked. What none of them can see is the class of defect that
// lives between two answers, where each endpoint is individually right and the set of them is not:
// a listing that shows what the by-id read refuses, a view derived from runs that names a run both
// of them hid, a refusal that tells an operator nothing they can act on.
//
// Every one of those has shipped. They are invisible to a test of one endpoint by construction, and
// they are what an operator meets first, because they are the paths a person browses rather than
// the path a script drives.
//
// This runs against runs the install actually executed, through the relay, with tokens it minted.
// That matters: the same properties are checked in process by test/scenario, and this is the one
// place they are asked of a real deployment.
//
// The two implementations are deliberate and the duplication is the price of a decision stated at
// the top of this package: supertest believes nothing it cannot see from outside, so it imports
// nothing from the product. Sharing this code would dissolve that, because a defect in what it
// imported could mask itself in the harness.
//
// The cost is that the two lists can drift, and a property added in process would silently not
// exist against a cluster. So they name each other. A property added to test/scenario/invariants.go
// belongs here too unless it needs state only a fixture can seed, and one added here belongs there
// unless it needs a real deployment.
func (h *harness) phaseInvariants() error {
	const phase = "invariants"

	// Two operators who differ only in what has been delegated to them. Without a grant on the
	// install nothing is hidden from anybody and every property below holds vacuously, so the
	// phase would pass while checking nothing.
	alpha, alphaID, err := h.mintOperator("scoped-alpha")
	if err != nil {
		return fmt.Errorf("mint the granted operator: %w", err)
	}
	beta, _, err := h.mintOperator("scoped-beta")
	if err != nil {
		return fmt.Errorf("mint the ungranted operator: %w", err)
	}

	// Something has to be delegated, or nothing is hidden from anybody and every property below
	// holds over an empty set while reading as a pass. Whichever grantable object this install
	// happens to hold will do; the properties do not care which kind it is.
	delegated := ""
	for _, listing := range []struct{ path, member string }{
		{"/v1/projects", "projects"},
		{"/v1/inventories", "inventories"},
		{"/v1/credentials", "credentials"},
	} {
		ids, lerr := h.objectIDs(listing.path, listing.member)
		if lerr != nil || len(ids) == 0 {
			continue
		}
		if gerr := h.apiCall("POST", "/v1/grants", &h.human, map[string]any{
			"subject": alphaID, "object": ids[0], "access": "use",
		}, nil); gerr != nil {
			return fmt.Errorf("delegate %s to the granted operator: %w", ids[0], gerr)
		}
		delegated = ids[0]
		break
	}
	if delegated == "" {
		// Both tiers reach this phase through deployAcrossFleet, which creates a credential and an
		// inventory before the run that uses them, so there is always something to delegate. If
		// that stops being true the phase has nothing to hide from anybody and every property below
		// would hold over an install where nothing is hidden, which is a pass that decided nothing.
		return fmt.Errorf("the install holds no project, inventory or credential, so nothing can " +
			"be delegated and the properties below would hold vacuously")
	}
	h.pass(phase, "one object is delegated, so the properties below have something to decide",
		delegated)

	runs, err := h.objectIDs("/v1/runs?limit=200", "runs")
	if err != nil {
		return fmt.Errorf("read the run history: %w", err)
	}
	if len(runs) == 0 {
		return fmt.Errorf("the install holds no run, so every run-derived property would hold " +
			"over an empty set")
	}

	actors := []*actor{&h.human, alpha, beta}
	// The by-id reads each caller is refused, which is where an object-level refusal is written.
	refusedPaths := map[string][]string{}
	for _, who := range actors {
		for _, id := range runs {
			if !h.canRead(who, "/v1/runs/"+id) {
				refusedPaths[who.Name] = append(refusedPaths[who.Name], "/v1/runs/"+id)
			}
		}
	}
	h.checkListFetchParity(phase, actors, runs)
	h.checkDerivedViewsAgree(phase, actors, runs)
	h.checkNoInternalErrors(phase, actors)
	h.checkRefusalsExplainThemselves(phase, actors, refusedPaths)
	h.checkUnauthenticatedReadsNothing(phase)
	h.checkNoCredentialMaterialEchoed(phase, actors)
	return nil
}

// checkListFetchParity requires the run listing and the by-id read behind it to give one answer.
//
// A list that returns a run the fetch refuses discloses that run, with its command, its variables
// and the credentials it named. A list that drops a run the fetch serves loses it, and an empty
// list reads as an install with no work in it rather than as an error, so nobody investigates.
// Both directions have shipped, the second one this morning.
func (h *harness) checkListFetchParity(phase string, actors []*actor, runs []string) {
	for _, who := range actors {
		listed, err := h.objectIDs("/v1/runs?limit=200", "runs")
		if err != nil {
			h.fail(phase, "the run list answers "+who.Name, err)
			continue
		}
		if who != &h.human {
			listed, err = h.objectIDsAs(who, "/v1/runs?limit=200", "runs")
			if err != nil {
				h.fail(phase, "the run list answers "+who.Name, err)
				continue
			}
		}
		inList := map[string]bool{}
		for _, id := range listed {
			inList[id] = true
		}
		mismatch := ""
		for _, id := range runs {
			served := h.canRead(who, "/v1/runs/"+id)
			if served != inList[id] {
				mismatch = fmt.Sprintf("%s: the fetch says %v and the list says %v",
					id, served, inList[id])
				break
			}
		}
		if mismatch != "" {
			h.fail(phase, "the run list and the run fetch agree for "+who.Name,
				fmt.Errorf("%s", mismatch))
			continue
		}
		h.pass(phase, "the run list and the run fetch agree for "+who.Name,
			fmt.Sprintf("%d run(s) compared", len(runs)))
	}
}

// checkDerivedViewsAgree requires no view built out of runs to name a run its own by-id read
// refuses to the same caller. Fleet health, the estate and a host's history are all computed from
// runs, and filtering the list while leaving these open returns exactly the rows the filter existed
// to withhold.
func (h *harness) checkDerivedViewsAgree(phase string, actors []*actor, runs []string) {
	hosts, err := h.fleetHosts()
	if err != nil {
		h.fail(phase, "the fleet view answers", err)
		return
	}
	paths := []string{"/v1/fleet", "/v1/estate", "/v1/drift", "/v1/changes"}
	for _, host := range hosts {
		paths = append(paths, "/v1/hosts/"+host+"/runs")
	}
	for _, who := range actors {
		refused := map[string]bool{}
		for _, id := range runs {
			if !h.canRead(who, "/v1/runs/"+id) {
				refused[id] = true
			}
		}
		if len(refused) == 0 {
			h.pass(phase, "no run is hidden from "+who.Name+", so the derived views may name any",
				"")
			continue
		}
		leaked := ""
		for _, path := range paths {
			status, body := h.rawGet(who, path)
			if status != http.StatusOK {
				continue
			}
			for id := range refused {
				if strings.Contains(body, `"`+id+`"`) {
					leaked = fmt.Sprintf("%s names %s, whose by-id fetch refuses %s",
						path, id, who.Name)
					break
				}
			}
			if leaked != "" {
				break
			}
		}
		if leaked != "" {
			h.fail(phase, "the derived views agree with the run fetch for "+who.Name,
				fmt.Errorf("%s", leaked))
			continue
		}
		h.pass(phase, "the derived views agree with the run fetch for "+who.Name,
			fmt.Sprintf("%d refused run(s), %d view(s)", len(refused), len(paths)))
	}
}

// invariantPaths are the reads the two status properties below are asked of, including ids nobody
// minted: a read that only ever meets rows that exist has never been asked what it does with a miss.
var invariantPaths = []string{
	"/v1/runs?limit=200", "/v1/projects", "/v1/credentials", "/v1/templates", "/v1/schedules",
	"/v1/inventories", "/v1/triggers", "/v1/orgs", "/v1/teams", "/v1/users", "/v1/grants",
	"/v1/audit", "/v1/doctor", "/v1/workers", "/v1/fleet", "/v1/drift", "/v1/changes",
	"/v1/estate", "/v1/tasks",
	"/v1/runs/run_no_such_thing", "/v1/schedules/sched_no_such_thing",
}

// checkNoInternalErrors requires every read to answer something other than a server error. A 500 is
// never a correct answer to a well formed read: it tells an operator something broke without
// telling them what, which is the dead end this product exists to stop producing.
func (h *harness) checkNoInternalErrors(phase string, actors []*actor) {
	for _, who := range actors {
		broke := ""
		for _, path := range invariantPaths {
			status, body := h.rawGet(who, path)
			// Zero is not a status. It is the request never arriving, which the port forward this
			// harness runs over can produce at any moment, and reading it as "not a server error"
			// made this and three properties beside it report green over an install nobody reached.
			if status == 0 {
				broke = fmt.Sprintf("%s could not be reached at all: %s", path, oneLine(body))
				break
			}
			if status >= 500 {
				broke = fmt.Sprintf("%s answered %d: %s", path, status, oneLine(body))
				break
			}
		}
		if broke != "" {
			h.fail(phase, "no read answers a server error for "+who.Name, fmt.Errorf("%s", broke))
			continue
		}
		h.pass(phase, "no read answers a server error for "+who.Name,
			fmt.Sprintf("%d path(s)", len(invariantPaths)))
	}
}

// checkRefusalsExplainThemselves requires every refusal to carry a reason. A 4xx with an empty body
// is a dead end: the caller learns they were stopped and not what to do about it, and the one
// explanation that is true is the only one they can act on.
func (h *harness) checkRefusalsExplainThemselves(phase string, actors []*actor, refused map[string][]string) {
	for _, who := range actors {
		silent := ""
		// The listings above are refused by the role gate. An object-level refusal happens on the
		// by-id read of something delegated elsewhere, and that is a different responder writing a
		// different body, so a check that never reaches one is a check that cannot see it. Breaking
		// that responder deliberately left this property green until these paths were added.
		paths := append(append([]string{}, invariantPaths...), refused[who.Name]...)
		for _, path := range paths {
			status, body := h.rawGet(who, path)
			if status == 0 {
				silent = fmt.Sprintf("%s could not be reached at all: %s", path, oneLine(body))
				break
			}
			if status < 400 || status >= 500 {
				continue
			}
			var doc struct {
				Error string `json:"error"`
			}
			if json.Unmarshal([]byte(body), &doc) != nil || strings.TrimSpace(doc.Error) == "" {
				silent = fmt.Sprintf("%s refused with %d and no reason: %s",
					path, status, oneLine(body))
				break
			}
		}
		if silent != "" {
			h.fail(phase, "every refusal explains itself to "+who.Name, fmt.Errorf("%s", silent))
			continue
		}
		h.pass(phase, "every refusal explains itself to "+who.Name, "")
	}
}

// checkUnauthenticatedReadsNothing requires every read to refuse a caller with no credentials at
// all. A caller with no token is not a caller with a role, and a read that answers one is the whole
// authorization layer reachable around rather than through.
func (h *harness) checkUnauthenticatedReadsNothing(phase string) {
	served := ""
	for _, path := range invariantPaths {
		status, body := h.rawGet(nil, path)
		if status == 0 {
			served = fmt.Sprintf("%s could not be reached at all, so a refusal was never "+
				"observed: %s", path, oneLine(body))
			break
		}
		if status == http.StatusOK {
			served = fmt.Sprintf("%s answered 200 with no credentials: %s", path, oneLine(body))
			break
		}
	}
	if served != "" {
		h.fail(phase, "no read answers a caller with no credentials", fmt.Errorf("%s", served))
		return
	}
	h.pass(phase, "no read answers a caller with no credentials",
		fmt.Sprintf("%d path(s)", len(invariantPaths)))
}

// checkNoCredentialMaterialEchoed requires no response to carry the private key the install was
// given to keep.
//
// This is the third shape of this property, and the first that is about a secret this install
// actually holds. It hunted bearer tokens, which are returned once at mint and only hashed
// afterward, so it scanned for a string the install does not possess and could never fail. It then
// hunted the audit signing seed, which only the Team, HA and disaster-recovery installs are
// configured with, so at this point in the run it was scanning for a string that install has never
// seen either. Both read as coverage and were none.
//
// The SSH private key is different: the harness wrote it, handed it over as a credential, and the
// install sealed and kept it. Every run the fleet executes uses it. It is exactly the material the
// product exists to hold without ever handing back, and the harness holds the plaintext to compare
// against, which is what makes the question answerable at all.
func (h *harness) checkNoCredentialMaterialEchoed(phase string, actors []*actor) {
	key, err := os.ReadFile(filepath.Join(h.work, "fleet_ed25519"))
	if err != nil {
		h.fail(phase, "the install holds credential material to hunt for",
			fmt.Errorf("the fleet key this install was given is unreadable here, so there is "+
				"nothing to compare a response against: %w", err))
		return
	}
	// The key's own base64 body, armour and line breaks removed, and a needle taken from its tail.
	//
	// The first attempt took "the longest non-armour line", which is wrong in a way that reads as
	// right. Every base64 line of an unencrypted ed25519 key is exactly seventy characters, so a
	// strict greater-than never advances past the first one, and that line is the OpenSSH container
	// header: byte for byte identical in every such key ever generated, carrying nothing of this
	// one. The check would have reported the same verdict whichever key the install held.
	//
	// The tail is past the header and the public half both, so it is this key's private scalar and
	// its comment. Hunting the joined body as well as the file as written means a response that
	// re-wraps the key at a different width, which is what any re-encoding produces, cannot slip
	// through a line-oriented comparison.
	var body strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(string(key)), "\n") {
		if !strings.HasPrefix(line, "-----") {
			body.WriteString(strings.TrimSpace(line))
		}
	}
	joined := body.String()
	const headerAndPublic = 160
	if len(joined) < headerAndPublic+64 {
		h.fail(phase, "the install holds credential material to hunt for",
			fmt.Errorf("the key body is %d characters, too short to take a needle from past its "+
				"container header, so a scan for it would prove nothing", len(joined)))
		return
	}
	needle := joined[len(joined)-64:]

	// A detector is shown to detect, against a leak this builds itself rather than against a value
	// it just read out of the response it is testing. The proof this replaced asked whether a
	// listing contained an id parsed out of that same listing, which it always does.
	for _, shape := range []struct {
		name string
		body string
	}{
		{"the key verbatim", `{"secret":"` + string(key) + `"}`},
		{"the key re-wrapped", `{"secret":"` + joined + `"}`},
	} {
		if !strings.Contains(shape.body, needle) {
			h.fail(phase, "the credential hunt can see a leak it is shown",
				fmt.Errorf("a response carrying %s is not detected, so an absence this reports "+
					"proves nothing", shape.name))
			return
		}
	}
	h.pass(phase, "the credential hunt can see a leak it is shown",
		"verbatim and re-wrapped, 64 characters from past the container header")

	for _, who := range actors {
		echoed := ""
		for _, path := range invariantPaths {
			status, body := h.rawGet(who, path)
			if status == 0 {
				echoed = fmt.Sprintf("%s could not be reached at all (%s), so this reports an "+
					"absence over a response nobody received", path, oneLine(body))
				break
			}
			if strings.Contains(body, needle) {
				echoed = fmt.Sprintf("%s returns the fleet key's private material to %s",
					path, who.Name)
				break
			}
		}
		if echoed != "" {
			h.fail(phase, "no response carries credential material to "+who.Name,
				fmt.Errorf("%s", echoed))
			continue
		}
		h.pass(phase, "no response carries credential material to "+who.Name,
			fmt.Sprintf("%d path(s)", len(invariantPaths)))
	}
}

// mintOperator creates an operator account and a token for it, as the admin, which is itself an
// assertion that the admin can.
// It returns the account's id as well as the actor, because a grant is written against the id and
// not the username: the API refuses a subject that is not a user or team id, so passing the name
// reads as a delegation and lands as a four hundred.
func (h *harness) mintOperator(name string) (*actor, string, error) {
	var created struct {
		ID string `json:"id"`
	}
	if err := h.apiCall("POST", "/v1/users", &h.human, map[string]any{
		"username": name, "password": "supertest-not-a-secret", "role": "operator",
	}, &created); err != nil {
		return nil, "", fmt.Errorf("create %s: %w", name, err)
	}
	if created.ID == "" {
		return nil, "", fmt.Errorf("creating %s returned no id, so nothing can be granted to it", name)
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err := h.apiCall("POST", "/v1/tokens", &h.human, map[string]any{
		"name": "supertest-" + name, "username": name,
	}, &minted); err != nil {
		return nil, "", fmt.Errorf("mint a token for %s: %w", name, err)
	}
	return &actor{Name: name, Type: "user", Token: minted.Token}, created.ID, nil
}

// rawGet issues one read and returns its status and body without treating a refusal as a failure,
// which is what the properties above need: a 403 is an answer to compare, not an error to stop on.
func (h *harness) rawGet(who *actor, path string) (int, string) {
	req, err := http.NewRequest("GET", h.api+path, nil)
	if err != nil {
		return 0, err.Error()
	}
	if who != nil {
		req.Header.Set("X-Switchtender-Actor", who.Name)
		req.Header.Set("X-Switchtender-Actor-Type", who.Type)
		if who.Token != "" {
			req.Header.Set("Authorization", "Bearer "+who.Token)
		}
	}
	resp, err := h.httpc.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err.Error()
	}
	return resp.StatusCode, string(raw)
}

// canRead reports whether this caller may read the object at path.
func (h *harness) canRead(who *actor, path string) bool {
	status, _ := h.rawGet(who, path)
	return status == http.StatusOK
}

// objectIDs reads the ids of a listing as the admin.
func (h *harness) objectIDs(path, member string) ([]string, error) {
	return h.objectIDsAs(&h.human, path, member)
}

// objectIDsAs reads the ids of a listing as the given caller.
func (h *harness) objectIDsAs(who *actor, path, member string) ([]string, error) {
	status, body := h.rawGet(who, path)
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d: %s", path, status, oneLine(body))
	}
	// Decoded one member at a time. A listing carries counts, summaries and cursors beside its
	// rows, so a map of arrays fails on the first real response.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if raw, ok := envelope[member]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, fmt.Errorf("decode %s of %s: %w", member, path, err)
		}
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.ID != "" {
			out = append(out, row.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// fleetHosts reads the hosts the install has seen, so each one's history is asked about too.
func (h *harness) fleetHosts() ([]string, error) {
	status, body := h.rawGet(&h.human, "/v1/fleet")
	if status != http.StatusOK {
		return nil, fmt.Errorf("/v1/fleet answered %d: %s", status, oneLine(body))
	}
	var doc struct {
		Hosts []struct {
			Host string `json:"host"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return nil, fmt.Errorf("decode /v1/fleet: %w", err)
	}
	out := make([]string, 0, len(doc.Hosts))
	for _, row := range doc.Hosts {
		if row.Host != "" {
			out = append(out, row.Host)
		}
	}
	return out, nil
}
