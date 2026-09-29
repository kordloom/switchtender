package importer

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
)

// cronEnvAssignment matches a crontab environment line such as PATH=/usr/bin or MAILTO=root, which
// sets context for the jobs below it rather than being a job itself.
var cronEnvAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\s*=`)

// FromCron returns a mapper that reads a crontab and plans one governed schedule per job line, each
// a single bash step running the line's command against the given inventory. It is the zero-history
// entry point for the most common automation state in the wild, a scattered crontab, turning an
// untracked cron job into an audited, approvable, host-history-tracked scheduled run.
//
// A crontab names no target host, so the inventory is supplied by the caller; without one the
// imported schedules would run against nothing, which is warned. When system is set the six-field
// /etc/crontab form is parsed, whose user column sits between the schedule and the command; the user
// is surfaced as a warning rather than silently run as whoever the server runs as. @reboot has no
// time-based cadence and is skipped with a warning, as is an environment assignment, which cannot be
// reproduced per schedule.
func FromCron(inventory string, system bool) func([]byte, time.Time) (*Plan, error) {
	return func(data []byte, now time.Time) (*Plan, error) {
		p := &Plan{}
		// A crontab line ran on the machine it was taken from. Imported, it becomes a shell step, and a
		// shell step runs where SwitchTender runs. Naming an inventory does not move it: an inventory
		// is what an Ansible step targets, and this import produces a shell step. The failure mode is a
		// command running in the wrong place and reporting success, so it is said plainly, once, whether
		// or not an inventory was named.
		p.warn("each imported line runs on the SwitchTender host, not on the machine this crontab " +
			"came from. To run one on other hosts, change its step to Ansible and target an inventory.")
		if strings.TrimSpace(inventory) == "" {
			p.warn("no --inventory was given, so imported schedules name no target host and run " +
				"against nothing until one is set on each")
		}
		s := bufio.NewScanner(bytes.NewReader(data))
		s.Buffer(make([]byte, 0, 64*1024), 1<<20)
		lineNo := 0
		for s.Scan() {
			lineNo++
			raw := strings.TrimSpace(s.Text())
			if raw == "" || strings.HasPrefix(raw, "#") {
				continue
			}
			if cronEnvAssignment.MatchString(raw) {
				p.warn("line %d sets an environment variable (%s), which imported schedules do not "+
					"carry; set it in the run or the inventory instead", lineNo, strings.SplitN(raw, "=", 2)[0])
				continue
			}
			expr, user, command, ok := splitCronLine(raw, system)
			if !ok {
				p.warn("line %d is not a schedule and was skipped: %q", lineNo, clipLine(raw))
				continue
			}
			if strings.HasPrefix(expr, "@reboot") {
				p.warn("line %d uses @reboot, which has no time-based equivalent and was skipped", lineNo)
				continue
			}
			if user != "" {
				p.warn("line %d ran as user %q; the imported schedule runs under the server's "+
					"execution account, so confirm that is equivalent", lineNo, user)
			}
			p.addSchedule(&schedule.Schedule{
				ID: schedule.NewID(), Name: fmt.Sprintf("cron line %d", lineNo), Cron: expr,
				Inventory: inventory, Enabled: true, CreatedAt: now,
				Steps: []run.PipelineStep{{Name: "cron", Tool: run.ToolBash, Command: command}},
			}, "the crontab", now)
		}
		if err := s.Err(); err != nil {
			return nil, fmt.Errorf("read crontab: %w", err)
		}
		if err := p.requireObjects("cron lines"); err != nil {
			return nil, err
		}
		return p, nil
	}
}

// splitCronLine splits a crontab entry into its schedule expression, an optional user column, and
// its command. A @-macro is one field; an ordinary schedule is five. In the system form the user
// column sits between the schedule and the command. It reports ok=false when the line has no command
// left after the schedule, which is not a job.
func splitCronLine(raw string, system bool) (expr, user, command string, ok bool) {
	rest := raw
	if strings.HasPrefix(rest, "@") {
		expr, rest = cutField(rest)
	} else {
		for i := 0; i < 5; i++ {
			var f string
			if f, rest = cutField(rest); f == "" {
				return "", "", "", false
			}
			if expr == "" {
				expr = f
			} else {
				expr += " " + f
			}
		}
	}
	if system {
		user, rest = cutField(rest)
	}
	command = strings.TrimSpace(rest)
	if command == "" {
		return "", "", "", false
	}
	return expr, user, command, true
}

// cutField returns the first whitespace-delimited field of s and the remainder with leading
// whitespace trimmed.
func cutField(s string) (field, rest string) {
	s = strings.TrimLeft(s, " \t")
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimLeft(s[i:], " \t")
	}
	return s, ""
}

// clipLine shortens a crontab line for a warning so a long command does not flood the report.
func clipLine(s string) string {
	const limit = 60
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
