package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Tool is one callable operation exposed to an agent.
type Tool struct {
	// Name is the identifier the agent calls.
	Name string
	// Description tells the model what the tool does and, where it matters, what it cannot do.
	Description string
	// InputSchema is the JSON Schema for the tool's arguments.
	InputSchema map[string]any
	// Run executes the tool with the raw arguments and returns the text the model reads.
	Run func(ctx context.Context, args json.RawMessage) (string, error)
}

// Options configures the tool set.
type Options struct {
	// AllowAdhoc exposes the ad-hoc run tool, which submits a playbook or command the agent composes
	// rather than one a person defined as a template. It is off by default: a template is a menu an
	// operator wrote, and keeping an agent on that menu is a far narrower surface than letting it
	// author the work. Approval policy applies either way, so this widens what can be proposed, not
	// what can execute unreviewed.
	AllowAdhoc bool
}

// Tools returns the tool set an agent may call, in listing order.
//
// The set is proposing runs and reading what happened. There is no approve or reject tool, so an
// agent cannot release its own work no matter how it is prompted, and no credential, account, token,
// grant, or policy tool, so it cannot widen its own reach. Everything here is an authenticated call
// the server authorizes and records like any other.
func Tools(c *Client, opts Options) []Tool {
	tools := []Tool{
		{
			Name: "list_templates",
			Description: "List the job templates this token may see. A template is a run an operator " +
				"has already defined and vetted. Start here to find what can be proposed.",
			InputSchema: object(nil, nil),
			Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
				var out any
				if err := c.do(ctx, "GET", "/v1/templates", nil, &out); err != nil {
					return "", err
				}
				return render(out)
			},
		},
		{
			Name: "propose_run",
			Description: "Propose a run by launching a job template. The run is submitted for the " +
				"account this token belongs to and is recorded in the tamper-evident audit chain " +
				"before it executes. If an approval policy covers it, it is held until a person " +
				"releases it; you cannot approve it yourself. Returns the created run, whose status " +
				"tells you whether it is running or awaiting approval.",
			InputSchema: object(map[string]any{
				"template_id": prop("string", "The template to launch, from list_templates."),
				"extra_vars": map[string]any{
					"type":        "object",
					"description": "Variables merged over the template's own.",
				},
				"answers": map[string]any{
					"type":        "object",
					"description": "Survey answers, keyed by the survey field's variable name.",
				},
				"limit":   prop("string", "Narrow this launch to a host pattern."),
				"dry_run": prop("boolean", "Run in the tool's no-change mode."),
				"reason": prop("string",
					"Why this run is being proposed. Recorded as a label so the audit trail carries "+
						"the agent's stated intent alongside the change."),
			}, []string{"template_id"}),
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				var in struct {
					TemplateID string         `json:"template_id"`
					ExtraVars  map[string]any `json:"extra_vars,omitempty"`
					Answers    map[string]any `json:"answers,omitempty"`
					Limit      string         `json:"limit,omitempty"`
					DryRun     *bool          `json:"dry_run,omitempty"`
					Reason     string         `json:"reason,omitempty"`
				}
				if err := decode(args, &in); err != nil {
					return "", err
				}
				if strings.TrimSpace(in.TemplateID) == "" {
					return "", fmt.Errorf("template_id is required")
				}
				body := map[string]any{}
				if len(in.ExtraVars) > 0 {
					body["extra_vars"] = in.ExtraVars
				}
				if len(in.Answers) > 0 {
					body["answers"] = in.Answers
				}
				if in.Limit != "" {
					body["limit"] = in.Limit
				}
				if in.DryRun != nil {
					body["dry_run"] = *in.DryRun
				}
				// The stated reason rides as a label so the change register shows why an agent asked,
				// not just that it did. Labels are non-secret, operator-visible metadata.
				labels := map[string]string{"proposed_by": "mcp"}
				if r := strings.TrimSpace(in.Reason); r != "" {
					labels["reason"] = clip(r, 200)
				}
				body["labels"] = labels

				var out any
				path := "/v1/templates/" + escapeID(in.TemplateID) + "/launch"
				if err := c.do(ctx, "POST", path, body, &out); err != nil {
					return "", err
				}
				return render(out)
			},
		},
		{
			Name: "get_run",
			Description: "Read one run's current state: status, exit code, timings, and what fired " +
				"it. A status of pending_approval means a person has not released it yet.",
			InputSchema: object(map[string]any{
				"run_id": prop("string", "The run to read."),
			}, []string{"run_id"}),
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				id, err := idArg(args, "run_id")
				if err != nil {
					return "", err
				}
				var out any
				if err := c.do(ctx, "GET", "/v1/runs/"+escapeID(id), nil, &out); err != nil {
					return "", err
				}
				return render(out)
			},
		},
		{
			Name: "get_run_log",
			Description: "Read a run's output. Secret values are redacted by the server before the " +
				"log is stored, so what you receive is already masked.",
			InputSchema: object(map[string]any{
				"run_id": prop("string", "The run whose output to read."),
			}, []string{"run_id"}),
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				id, err := idArg(args, "run_id")
				if err != nil {
					return "", err
				}
				var out any
				if err := c.do(ctx, "GET", "/v1/runs/"+escapeID(id)+"/logs", nil, &out); err != nil {
					return "", err
				}
				return render(out)
			},
		},
		{
			Name: "get_run_evidence",
			Description: "Read a run's evidence dossier: its spec, risk grade, approval decisions, " +
				"per-host outcomes, and the audit-chain receipts behind all of it. This is the " +
				"record an auditor is given, and it is what proves what actually happened.",
			InputSchema: object(map[string]any{
				"run_id": prop("string", "The run whose evidence to read."),
			}, []string{"run_id"}),
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				id, err := idArg(args, "run_id")
				if err != nil {
					return "", err
				}
				var out any
				if err := c.do(ctx, "GET", "/v1/runs/"+escapeID(id)+"/evidence", nil, &out); err != nil {
					return "", err
				}
				return render(out)
			},
		},
		{
			Name: "list_runs",
			Description: "List recent runs, newest first. Supports the same fielded search the " +
				"interface uses, for example 'status:failed host:web01 label:env=prod'.",
			InputSchema: object(map[string]any{
				"query": prop("string", "Fielded search terms. Optional."),
				"limit": prop("integer", "How many runs to return. Optional."),
			}, nil),
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				var in struct {
					Query string `json:"query,omitempty"`
					Limit int    `json:"limit,omitempty"`
				}
				if err := decode(args, &in); err != nil {
					return "", err
				}
				path := "/v1/runs"
				if q := listQuery(in.Query, in.Limit); q != "" {
					path += "?" + q
				}
				var out any
				if err := c.do(ctx, "GET", path, nil, &out); err != nil {
					return "", err
				}
				return render(out)
			},
		},
	}
	if opts.AllowAdhoc {
		tools = append(tools, adhocTool(c))
	}
	return tools
}

// adhocTool proposes a run the agent composes rather than one an operator defined as a template. It
// is registered only when the operator opted in, because it widens the agent's reach from a vetted
// menu to anything its token may submit. Approval policy still governs execution, and the submission
// is recorded before anything runs.
func adhocTool(c *Client) Tool {
	return Tool{
		Name: "propose_adhoc_run",
		Description: "Propose a run you compose yourself, rather than launching a template. Prefer " +
			"propose_run with a template when one fits. The run is recorded in the audit chain " +
			"before it executes and is held if an approval policy covers it; you cannot approve it.",
		InputSchema: object(map[string]any{
			"tool": prop("string",
				"Execution engine: ansible, terraform, opentofu, bash, powershell, python, or go."),
			"playbook":     prop("string", "Playbook path, for the ansible tool."),
			"command":      prop("string", "Script or working directory, for the non-ansible tools."),
			"inventory_id": prop("string", "Stored inventory to target."),
			"project_id":   prop("string", "Git project supplying the playbook."),
			"limit":        prop("string", "Narrow the run to a host pattern."),
			"dry_run":      prop("boolean", "Run in the tool's no-change mode."),
			"reason":       prop("string", "Why this run is being proposed. Recorded as a label."),
		}, nil),
		Run: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Tool        string `json:"tool,omitempty"`
				Playbook    string `json:"playbook,omitempty"`
				Command     string `json:"command,omitempty"`
				InventoryID string `json:"inventory_id,omitempty"`
				ProjectID   string `json:"project_id,omitempty"`
				Limit       string `json:"limit,omitempty"`
				DryRun      bool   `json:"dry_run,omitempty"`
				Reason      string `json:"reason,omitempty"`
			}
			if err := decode(args, &in); err != nil {
				return "", err
			}
			if strings.TrimSpace(in.Playbook) == "" && strings.TrimSpace(in.Command) == "" {
				return "", fmt.Errorf("a playbook or a command is required")
			}
			labels := map[string]string{"proposed_by": "mcp"}
			if r := strings.TrimSpace(in.Reason); r != "" {
				labels["reason"] = clip(r, 200)
			}
			body := map[string]any{"labels": labels}
			for key, value := range map[string]string{
				"tool": in.Tool, "playbook": in.Playbook, "command": in.Command,
				"inventory_id": in.InventoryID, "project_id": in.ProjectID, "limit": in.Limit,
			} {
				if value != "" {
					body[key] = value
				}
			}
			if in.DryRun {
				body["dry_run"] = true
			}
			var out any
			if err := c.do(ctx, "POST", "/v1/runs", body, &out); err != nil {
				return "", err
			}
			return render(out)
		},
	}
}
