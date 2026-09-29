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
		// The zone the lines below are read in. Empty means the server's local time, which is what a
		// crontab with no CRON_TZ means.
		zone := ""
		for s.Scan() {
			lineNo++
			raw := strings.TrimSpace(s.Text())
			if raw == "" || strings.HasPrefix(raw, "#") {
				continue
			}
			if cronEnvAssignment.MatchString(raw) {
				name, value, _ := strings.Cut(raw, "=")
				name, value = strings.TrimSpace(name), strings.Trim(strings.TrimSpace(value), `"'`)
				// CRON_TZ is not an ordinary variable. It reads the schedule of every line below it in
				// a different zone, so skipping it as environment carried every following job across
				// at the server's local time, under a warning that said an environment variable had
				// not come across. A nightly window moves by whole hours that way, and the report
				// named the wrong thing as the loss.
				if strings.EqualFold(name, "CRON_TZ") {
					if _, err := time.LoadLocation(value); err != nil {
						p.warn("line %d sets CRON_TZ to %q, which is not a zone this system knows, so "+
							"the schedules below it import at the server's local time: %v",
							lineNo, clipLine(value), err)
						zone = ""
						continue
					}
					zone = value
					continue
				}
				p.warn("line %d sets an environment variable (%s), which imported schedules do not "+
					"carry; set it in the run or the inventory instead", lineNo, name)
				continue
			}
			expr, user, command, ok := splitCronLine(raw, system)
			if !ok {
				p.warn("line %d is not a schedule and was skipped: %q", lineNo, clipLine(raw))
				continue
			}
			if why, diverges := vixieDayFieldsDiverge(expr); diverges {
				p.warn("line %d was not imported: %s", lineNo, why)
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
			// Vixie cron accepts 7 for Sunday and plenty of crontabs use it; the parser this product
			// schedules with caps the field at 6 and refuses the line, so every Sunday job was
			// dropped with a warning that did not say a weekly backup had not come across.
			p.addSchedule(&schedule.Schedule{
				ID: schedule.NewID(), Name: fmt.Sprintf("cron line %d", lineNo),
				Cron: StandardizeCron(expr), Timezone: zone,
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

// vixieDayFieldsDiverge reports whether a crontab line's two day fields combine differently here than
// they do in the cron that wrote it.
//
// Cron treats the day of month and the day of week as alternatives when both are restricted, and as
// requirements when either is not. What decides "restricted" is the difference. Vixie asks whether the
// field text begins with a star, so "*/2" is unrestricted to it and the pair is ANDed. The parser this
// product schedules with asks whether the field is a star, so "*/2" is a restriction and the pair is
// ORed.
//
// So "0 0 1 * */2" means the first of the month, and only when that day is an even weekday. Imported,
// it meant every first of the month and also every even weekday, which fires roughly fifteen times as
// often and on days the crontab never would. Nothing said so: the expression came across character for
// character and read correctly to anyone checking it.
//
// There is no faithful way to write the ANDed pair as one expression a parser that ORs will read
// correctly, and every approximation fires on days the operator did not choose. So the line is refused
// and named, which is the rule this importer already follows for a workflow whose nodes disagree about
// their limit or their inventory.
func vixieDayFieldsDiverge(expr string) (string, bool) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return "", false
	}
	dom, dow := fields[2], fields[4]
	// Whether each cron considers the pair a requirement rather than a choice.
	vixieRequiresBoth := strings.HasPrefix(dom, "*") || strings.HasPrefix(dow, "*")
	hereRequiresBoth := dom == "*" || dow == "*"
	if vixieRequiresBoth == hereRequiresBoth {
		return "", false
	}
	return fmt.Sprintf("its day-of-month %q and day-of-week %q combine differently here than in the "+
		"cron that wrote it. A field beginning with a star is unrestricted to cron, so it requires "+
		"both days to match; a field that is not exactly a star is a restriction here, so either day "+
		"matching is enough. The line would fire on days it never has. Recreate it as a schedule that "+
		"restricts one day field and leaves the other a plain star.", oneLine(dom), oneLine(dow)), true
}
