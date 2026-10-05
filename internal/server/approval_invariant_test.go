package server

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/mcp"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// approvalPath is one place in the product that can satisfy an approval, release a held run, or
// record a decision about one, and the reason an agent identity cannot use it.
type approvalPath struct {
	// Surface is where the path is reached from: the API, a workflow step, the dispatcher, the
	// system itself, the demo, or the store contract.
	Surface string
	// Why says what stops an agent there, naming the test that proves it where one exists.
	Why string
}

// approvalPaths is every call site in the product's non-test code that reaches an approval
// primitive: deciding a held run or a workflow approval step, committing a decision to the chain,
// stamping an approved spec, settling an approval step, or moving a run out of pending_approval. It
// is the enumeration the invariant rests on. A new call site, which is a new way to release work,
// fails TestNoPathLetsAnAgentSatisfyAnApproval until it is written here with what stops an agent at
// it.
var approvalPaths = map[string]approvalPath{
	"internal/server/handlers_approvals.go:decideRun:Approve": {Surface: "API",
		Why: "POST /v1/runs/{id}/approve is an admin route an agent's capped token never reaches " +
			"(TestTheDoorAloneStopsAnAgentApprovingAHeldRun), and the dispatcher refuses an agent " +
			"decider on its own (TestTheDispatcherAloneStopsAnAgentApprovingAHeldRun)"},
	"internal/server/handlers_approvals.go:decideRun:DecideRun": {Surface: "API",
		Why: "the same route and the same dispatcher lock, carrying the approver's reason"},
	"internal/server/handlers_approvals.go:decideRun:Reject": {Surface: "API",
		Why: "a rejection releases nothing, and its route is admin like an approval's"},
	"internal/server/handlers_approvals.go:decideStep:DecideStep": {Surface: "workflow steps",
		Why: "the approve route refuses an agent at the door, decideStep refuses it again, and " +
			"DecideStep refuses an agent decider on its own"},
	"internal/dispatch/approvals.go:Dispatcher.Approve:approveRun": {Surface: "dispatcher",
		Why: "approveRun refuses an agent decider before it reads the run " +
			"(TestApproveRefusesAnAgentDeciderOnItsOwn)"},
	"internal/dispatch/reasons.go:Dispatcher.DecideRun:approveRun": {Surface: "dispatcher",
		Why: "the same approveRun and its agent check"},
	"internal/dispatch/approvals.go:Dispatcher.approveRun:DecisionEntry": {
		Surface: "dispatcher", Why: "reached only past approveRun's agent check"},
	"internal/dispatch/approvals.go:Dispatcher.rejectRun:DecisionEntry": {
		Surface: "dispatcher", Why: "prepares a rejection, which satisfies no approval"},
	"internal/dispatch/approvals_step.go:Dispatcher.DecideStep:StepDecisionEntry": {
		Surface: "workflow steps", Why: "reached only past DecideStep's agent check"},
	"internal/dispatch/approvals_step.go:Dispatcher.timeOutStep:StepDecisionEntry": {
		Surface: "system", Why: "prepares a timeout, which takes the deny path and approves nothing"},
	"internal/dispatch/decide.go:Dispatcher.decide:ClaimDecision": {Surface: "dispatcher",
		Why: "claims a decision approveRun, rejectRun, DecideStep, or timeOutStep built, each " +
			"past its own agent check, and is reached from nowhere else"},
	"internal/dispatch/decide.go:Dispatcher.finishDecision:SettleDecision": {Surface: "dispatcher",
		Why: "settles only a decision decide claimed, or one a janitor finds claimed, which a " +
			"claim made past an agent check is the only way to create"},
	"internal/dispatch/decide.go:Dispatcher.finishDecision:StampApprovedSpec": {
		Surface: "dispatcher", Why: "stamps what a claimed decision was made on, as its settle"},
	"internal/outcome/spec.go:CommitDecisionWith:DecisionEntry": {Surface: "dispatcher",
		Why: "the append-at-once form of DecisionEntry, called by nothing that decides"},
	"internal/outcome/approval.go:CommitStepDecisionWith:StepDecisionEntry": {Surface: "system",
		Why: "the append-at-once form used for a step's request"},
	"internal/storetest/contract_decision_claim.go:testClaimDecisionOnce:ClaimDecision": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_decision_claim.go:testClaimedRunIsFenced:ClaimDecision": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_decision_claim.go:testClaimedRunIsFenced:SettleHeld": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_decision_claim.go:testClaimedRunIsFenced:TransitionStatus": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_decision_claim.go:testClaimedRunIsFenced:" +
		"TransitionStatusAndClaim": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_decision_claim.go:testParkedIsNoHeldRun:ClaimDecision": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_decision_claim.go:testParkedIsNoHeldRun:SettleHeld": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_decision_claim.go:testParkedIsNoHeldRun:TransitionStatus": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_decision_claim.go:testParkedIsNoHeldRun:" +
		"TransitionStatusAndClaim": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_decision_claim.go:testSettleDecision:ClaimDecision": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_decision_claim.go:testSettleDecision:SettleDecision": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/dispatch/approvals.go:Dispatcher.startSplit:TransitionStatus": {
		Surface: "system", Why: "releases the shards of a split a person already approved through " +
			"approveRun, and is reached from nowhere else"},
	"internal/dispatch/approvals_step.go:Dispatcher.openApprovalStep:CommitStepDecision": {
		Surface: "system", Why: "records that a workflow reached an approval step, a request by the " +
			"system and never a decision"},
	"internal/dispatch/approvals_step.go:Dispatcher.openApprovalStep:SettleHeld": {
		Surface: "system", Why: "fails a step whose request could not be recorded, taking its deny " +
			"path"},
	"internal/dispatch/approvals_step.go:Dispatcher.resumeParked:TransitionStatusAndClaim": {
		Surface: "system", Why: "resumes a workflow only after DecideStep or timeOutStep settled its " +
			"step, and decides nothing itself"},
	"internal/outcome/spec.go:CommitDecision:CommitDecisionWith": {
		Surface: "dispatcher", Why: "the record-less form of CommitDecisionWith, called by nothing " +
			"that decides"},
	"internal/outcome/approval.go:CommitStepDecision:CommitStepDecisionWith": {
		Surface: "system", Why: "the record-less form used for a step's request and its timeout"},
	"internal/demo/demo.go:seedGovernance:Approve": {Surface: "demo",
		Why: "the read-only demo seeds a decision by a person account, and the dispatcher refuses " +
			"the call were it ever an agent's"},
	"internal/storetest/contract_approval.go:testSettleHeld:SettleHeld": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_sealed.go:testSealedMaterialWipes:SettleHeld": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_claim.go:testRunRacesUnderConcurrency:TransitionStatusAndClaim": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_runs.go:testTransitionStatus:TransitionStatus": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_runs.go:testTransitionStatusAndClaim:TransitionStatusAndClaim": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_ends.go:testEndLedger:SettleHeld": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_ends.go:testEndLedger:TransitionStatus": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
	"internal/storetest/contract_outcome.go:testOutcomeOwed:TransitionStatus": {
		Surface: "store contract", Why: "exercises the store with no decider and no route"},
}

// approvalPrimitives are the calls that decide, commit, or release. A transition is one only when
// it moves a run out of pending_approval, which is read from its from argument. A decision is
// prepared, claimed, and settled through calls of its own, and each of them is a primitive: the
// entry a claim carries is what the chain records, the claim is what wins the run, and the settle
// is what releases it.
var approvalPrimitives = map[string]bool{
	"Approve": true, "Reject": true, "DecideRun": true, "DecideStep": true, "approveRun": true,
	"CommitDecision": true, "CommitDecisionWith": true, "CommitStepDecision": true,
	"CommitStepDecisionWith": true, "SettleHeld": true, "StampApprovedSpec": true,
	"DecisionEntry": true, "StepDecisionEntry": true, "ClaimDecision": true,
	"SettleDecision": true,
}

// scanApprovalCalls walks every non-test Go file under root and returns each call site reaching an
// approval primitive, keyed as file:function:callee.
func scanApprovalCalls(t *testing.T, root string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "testdata", ".bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			for _, callee := range approvalCallees(fd) {
				found[filepath.ToSlash(rel)+":"+funcName(fd)+":"+callee] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan the source: %v", err)
	}
	return found
}

// funcName names a function declaration, with its receiver type for a method.
func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := fd.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// approvalCallees returns the approval primitives a function body calls.
func approvalCallees(fd *ast.FuncDecl) []string {
	var out []string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var callee string
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			callee = fn.Sel.Name
		case *ast.Ident:
			callee = fn.Name
		}
		if approvalPrimitives[callee] || releasesHeld(callee, call) {
			out = append(out, callee)
		}
		return true
	})
	return out
}

// releasesHeld reports whether a call is a status transition out of pending_approval.
func releasesHeld(callee string, call *ast.CallExpr) bool {
	if (callee != "TransitionStatus" && callee != "TransitionStatusAndClaim") || len(call.Args) < 3 {
		return false
	}
	switch from := call.Args[2].(type) {
	case *ast.SelectorExpr:
		return from.Sel.Name == "StatusPendingApproval"
	case *ast.Ident:
		return from.Name == "StatusPendingApproval"
	}
	return false
}

// unreadable marks a route whose pattern or handler the reader could not resolve. A route the
// invariant cannot read is a route it cannot check, so every check that reads routes fails on it.
const unreadable = "?"

// routeHandlers maps every route the package in dir registers, in any of its non-test files, to the
// handler it registers, read from its mux.Handle and mux.HandleFunc calls. root is the module root,
// where a constant another package of the module declares is read from.
//
// A pattern is read as the string it evaluates to: a literal, a constant of this package or of
// another in the module, or a concatenation of them, which is how the AWX-compatible callback
// address and the beat feed are registered. A handler is named by the function or method that
// builds it, so approveRunHandler(...) and callbacks.native() read as their builders, by the
// method a method value names, and, when it is a variable, by what the enclosing function last
// assigned to it. An inline function reads as func. A route whose pattern or handler cannot be read
// is kept under unreadable rather than dropped: the first version read one file and only literal
// patterns, so the AWX-compatible callback routes, registered from another file with a constant
// prefix and a handler variable, were invisible to the very check that keeps callbacks from
// deciding.
func routeHandlers(t *testing.T, root, dir string) map[string]string {
	t.Helper()
	consts := &moduleConsts{t: t, root: root, byPkg: map[string]map[string]string{}}
	files := packageFiles(t, dir)
	local := consts.declared(files)
	out := map[string]string{}
	for _, file := range files {
		imports := importNames(file)
		resolve := func(e ast.Expr) (string, bool) { return consts.eval(e, local, imports) }
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			assigned := map[string]ast.Expr{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
					for i, lhs := range as.Lhs {
						if id, ok := lhs.(*ast.Ident); ok {
							assigned[id.Name] = as.Rhs[i]
						}
					}
				}
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
					return true
				}
				pattern, ok := resolve(call.Args[0])
				if !ok {
					pattern = fmt.Sprintf("%s %T", unreadable, call.Args[0])
				}
				out[pattern] = handlerName(call.Args[1], assigned, 0)
				return true
			})
		}
	}
	return out
}

// packageFiles parses every non-test Go file in dir.
func packageFiles(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out = append(out, file)
	}
	return out
}

// importNames maps each name a file refers to an import by to the import's path.
func importNames(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path[strings.LastIndexByte(path, '/')+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		out[name] = path
	}
	return out
}

// modulePath is the import path every package of this module sits under.
const modulePath = "github.com/kordloom/switchtender/"

// moduleConsts reads the string constants packages of this module declare, each package once.
type moduleConsts struct {
	// t fails the test when a package cannot be read.
	t *testing.T
	// root is the module root.
	root string
	// byPkg caches each package's constants by import path.
	byPkg map[string]map[string]string
}

// declared evaluates the string constants files declare, resolving ones that name each other or
// another package's in any order.
func (m *moduleConsts) declared(files []*ast.File) map[string]string {
	out := map[string]string{}
	for pass, changed := 0, true; changed && pass < 8; pass++ {
		changed = false
		for _, file := range files {
			imports := importNames(file)
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if _, done := out[name.Name]; done || i >= len(vs.Values) {
							continue
						}
						if v, ok := m.eval(vs.Values[i], out, imports); ok {
							out[name.Name], changed = v, true
						}
					}
				}
			}
		}
	}
	return out
}

// eval evaluates a constant string expression: a literal, a constant named in local, a constant of
// another package of the module, a parenthesized one, or a concatenation of them.
func (m *moduleConsts) eval(e ast.Expr, local, imports map[string]string) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := local[v.Name]
		return s, ok
	case *ast.SelectorExpr:
		pkg, ok := v.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		s, ok := m.of(imports[pkg.Name])[v.Sel.Name]
		return s, ok
	case *ast.ParenExpr:
		return m.eval(v.X, local, imports)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		left, lok := m.eval(v.X, local, imports)
		right, rok := m.eval(v.Y, local, imports)
		return left + right, lok && rok
	}
	return "", false
}

// of returns the string constants of the module package at importPath, none for a package
// outside the module.
func (m *moduleConsts) of(importPath string) map[string]string {
	rel, ok := strings.CutPrefix(importPath, modulePath)
	if !ok {
		return nil
	}
	if got, ok := m.byPkg[importPath]; ok {
		return got
	}
	m.byPkg[importPath] = map[string]string{}
	got := m.declared(packageFiles(m.t, filepath.Join(m.root, filepath.FromSlash(rel))))
	m.byPkg[importPath] = got
	return got
}

// handlerName names the handler a route registers, resolving a variable through what the
// enclosing function last assigned to it.
func handlerName(e ast.Expr, assigned map[string]ast.Expr, depth int) string {
	switch h := e.(type) {
	case *ast.CallExpr:
		switch fun := h.Fun.(type) {
		case *ast.Ident:
			return fun.Name
		case *ast.SelectorExpr:
			return fun.Sel.Name
		}
	case *ast.SelectorExpr:
		return h.Sel.Name
	case *ast.FuncLit:
		return "func"
	case *ast.Ident:
		if rhs, ok := assigned[h.Name]; ok && depth < 8 {
			return handlerName(rhs, assigned, depth+1)
		}
	}
	return unreadable
}

// decisionHandlers are the server handlers that decide or annotate a decision.
var decisionHandlers = map[string]bool{
	"approveRunHandler": true, "rejectRunHandler": true, "addCorrectionHandler": true,
	"redactReasonHandler": true,
}

// decisionWords mark a route or a tool that would decide something.
var decisionWords = regexp.MustCompile(`(?i)approve|reject|decide|decision|deny|release`)

// TestNoPathLetsAnAgentSatisfyAnApproval is the invariant: approvals come from a person's session
// or a person's own token, never an agent's, on every path into the product. It enumerates the
// paths from the source rather than from a list someone keeps, so a new way to release work, a new
// route that reaches a decision, a new MCP tool, a new relay route, or a callback or review path
// that starts deciding fails the build here until it is registered with what stops an agent at it.
//
//nolint:funlen // Test function.
func TestNoPathLetsAnAgentSatisfyAnApproval(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")

	t.Run("every call site that can release work is registered", func(t *testing.T) {
		t.Parallel()
		found := scanApprovalCalls(t, root)
		var unregistered, stale []string
		for key := range found {
			if _, ok := approvalPaths[key]; !ok {
				unregistered = append(unregistered, key)
			}
		}
		for key := range approvalPaths {
			if !found[key] {
				stale = append(stale, key)
			}
		}
		sort.Strings(unregistered)
		sort.Strings(stale)
		for _, key := range unregistered {
			t.Errorf("%s reaches an approval primitive and is not in approvalPaths: register it with "+
				"what stops an agent there, and a test proving it", key)
		}
		for _, key := range stale {
			t.Errorf("approvalPaths lists %s, which the source no longer contains", key)
		}
		for key, p := range approvalPaths {
			if p.Surface == "" || p.Why == "" {
				t.Errorf("approvalPaths entry %s does not say what stops an agent", key)
			}
		}
	})

	const apiCase = "API: every route that decides is admin and an agent is capped below it"
	t.Run(apiCase, func(t *testing.T) {
		t.Parallel()
		if got := user.AgentRole(user.RoleAdmin); got == user.RoleAdmin {
			t.Fatalf("an agent's token bound to an admin carries %q, so the door does not hold", got)
		}
		routes := routeHandlers(t, root, ".")
		var deciding []string
		for pattern, handler := range routes {
			if !decisionHandlers[handler] {
				continue
			}
			deciding = append(deciding, pattern)
			method, path, _ := strings.Cut(pattern, " ")
			concrete := regexp.MustCompile(`\{[a-z]+\}`).ReplaceAllString(path, "x")
			if got := requiredRole(httptest.NewRequest(method, concrete, nil)); got != user.RoleAdmin {
				t.Errorf("%s decides and requires %q, so an agent capped at %q reaches it", pattern,
					got, user.AgentRole(user.RoleAdmin))
			}
		}
		if len(deciding) != len(decisionHandlers) {
			t.Errorf("found %d deciding routes %v, want %d: a deciding handler moved or a new one "+
				"was registered without being named here", len(deciding), deciding,
				len(decisionHandlers))
		}
		for pattern, handler := range routes {
			if decisionHandlers[handler] || !strings.Contains(pattern, "POST") {
				continue
			}
			if strings.Contains(pattern, "/approve") || strings.Contains(pattern, "/reject") {
				t.Errorf("%s reads as a decision route but is served by %s, which this invariant "+
					"does not know", pattern, handler)
			}
		}
	})

	t.Run("API: an agent's token is refused on a held run and a workflow step", func(t *testing.T) {
		t.Parallel()
		h := newStepHarness(t)
		held := &run.Run{ID: run.NewID(), Tool: run.ToolBash, Command: "deploy",
			Queue: "served-by-nobody", Status: run.StatusPendingApproval, CreatedAt: time.Now()}
		if err := h.store.Save(context.Background(), held); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		if code, body := h.call(t, h.agent, http.MethodPost, "/v1/runs/"+held.ID+"/approve",
			""); code != http.StatusForbidden {
			t.Errorf("agent approving a held run = %d %s, want 403", code, body)
		}
		_, step := h.submitGated(t, h.agent)
		if code, body := h.call(t, h.agent, http.MethodPost, "/v1/runs/"+step.ID+"/approve",
			`{"state_digest":"`+step.StateDigest+`"}`); code != http.StatusForbidden {
			t.Errorf("agent approving a workflow step = %d %s, want 403", code, body)
		}
	})

	t.Run("dispatcher: an agent decider is refused for a run and a step", func(t *testing.T) {
		t.Parallel()
		h := newStepHarness(t)
		agent := outcome.Decider{Name: "deploy-bot", Type: actorTypeAgent, OnBehalfOf: "owner"}
		d := h.d
		held := &run.Run{ID: run.NewID(), Tool: run.ToolBash, Command: "deploy",
			Queue: "served-by-nobody", Status: run.StatusPendingApproval, CreatedAt: time.Now()}
		if err := h.store.Save(context.Background(), held); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		if _, err := d.Approve(context.Background(), held.ID, agent); !errors.Is(err,
			dispatch.ErrAgentApproval) {
			t.Errorf("Approve() as an agent = %v, want ErrAgentApproval", err)
		}
		_, step := h.submitGated(t, h.admin)
		if _, err := d.DecideStep(context.Background(), step.ID, dispatch.StepDecision{Approve: true,
			By: agent}); !errors.Is(err, dispatch.ErrAgentApproval) {
			t.Errorf("DecideStep() as an agent = %v, want ErrAgentApproval", err)
		}
	})

	t.Run("MCP: no tool decides and no tool reaches a decision route", func(t *testing.T) {
		t.Parallel()
		for _, tool := range mcp.Tools(nil, mcp.Options{AllowAdhoc: true}) {
			if decisionWords.MatchString(tool.Name) {
				t.Errorf("MCP tool %q reads as a decision", tool.Name)
			}
		}
		for _, lit := range sourceStrings(t, filepath.Join(root, "internal", "mcp")) {
			if strings.Contains(lit, "/approve") || strings.Contains(lit, "/reject") ||
				strings.Contains(lit, "/decisions") {
				t.Errorf("the MCP client names a decision route in %q", lit)
			}
		}
	})

	t.Run("callbacks, relay proposals, and PR review reach no decision", func(t *testing.T) {
		t.Parallel()
		relay := routeHandlers(t, root, filepath.Join(root, "internal", "relay"))
		if len(relay) < 5 {
			t.Fatalf("only %d relay routes parsed; the registration shape changed", len(relay))
		}
		for _, problem := range callbackSurfaceProblems(scanApprovalCalls(t, root),
			routeHandlers(t, root, "."), relay) {
			t.Error(problem)
		}
	})
}

// callbackSurfaces are the files and packages that serve callers with no person behind them: a
// host's provisioning callback, on its own address and the AWX-compatible one, a relay worker, a
// pull request review, and a webhook trigger.
var callbackSurfaces = []string{"internal/server/callback_handlers.go",
	"internal/server/awx_callback_handlers.go", "internal/relay/", "internal/review/",
	"internal/server/review_hook.go", "internal/server/trigger_handlers.go"}

// callbackRoutes are the callback routes the invariant has to see to be checking anything.
var callbackRoutes = []string{"POST /v1/templates/{id}/callback",
	"POST /api/v2/job_templates/{id}/callback", "POST /api/v2/job_templates/{id}/callback/{$}"}

// callbackSurfaceProblems reports every way a caller with no person behind it reaches a decision:
// an approval primitive called from a callback surface, a callback or hook route served by a
// deciding handler, a relay route that reads as a decision, a route the reader could not resolve,
// and a callback route the reader cannot see at all. found is what scanApprovalCalls returned,
// routes the server package's routes, and relay the relay package's.
func callbackSurfaceProblems(found map[string]bool, routes, relay map[string]string) []string {
	var out []string
	for key := range found {
		for _, surface := range callbackSurfaces {
			if strings.HasPrefix(key, surface) {
				out = append(out, key+" reaches an approval primitive")
			}
		}
	}
	for pattern := range relay {
		if decisionWords.MatchString(pattern) || strings.HasPrefix(pattern, unreadable) {
			out = append(out, fmt.Sprintf("relay route %q reads as a decision a worker could "+
				"make, or cannot be read", pattern))
		}
	}
	for pattern, handler := range routes {
		if strings.HasPrefix(pattern, unreadable) || handler == unreadable {
			out = append(out, fmt.Sprintf("route %q served by %q cannot be read, so this "+
				"invariant cannot say what it serves", pattern, handler))
			continue
		}
		callbackOrHook := strings.Contains(pattern, "/callback") || strings.Contains(pattern, "/hooks/")
		if callbackOrHook && decisionHandlers[handler] {
			out = append(out, pattern+" is a callback or hook route served by a deciding handler")
		}
	}
	for _, want := range callbackRoutes {
		if _, ok := routes[want]; !ok {
			out = append(out, "the callback route "+want+" is not where this invariant looks for it")
		}
	}
	sort.Strings(out)
	return out
}

// sourceStrings returns every string literal in a package's non-test files, which holds every route
// a client package can call.
func sourceStrings(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, s)
			}
			return true
		})
	}
	return out
}
