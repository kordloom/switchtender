package attention

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The built-in thresholds, what an install with no thresholds file measures against.
const (
	// DefaultBlockedAfter is how long a run waits behind another run or a full worker before it is
	// shown as blocked. A run that starts within that time was queued, not stuck, and showing it
	// would bury the runs that are.
	DefaultBlockedAfter = 15 * time.Minute
	// DefaultAlertNoWorker is how long a run waits with no worker serving its queue before it
	// alerts.
	DefaultAlertNoWorker = 15 * time.Minute
	// DefaultAlertBlocked is how long a run stays blocked before it alerts.
	DefaultAlertBlocked = 15 * time.Minute
	// DefaultAlertWorkerLostLeases is how many lease periods may pass after a lost worker last
	// reported before its run alerts. The lease sweep reclaims such a run within one lease period
	// and one sweep, so a run still held past two has not been reclaimed when it should have been.
	DefaultAlertWorkerLostLeases = 2
)

// Setting is one threshold as a thresholds file states it: absent, off, or a duration.
type Setting struct {
	// set reports that the file stated this threshold.
	set bool
	// value is the duration, zero for off.
	value time.Duration
}

// Off returns a setting that turns its alert off.
func Off() Setting { return Setting{set: true} }

// After returns a setting of d.
func After(d time.Duration) Setting { return Setting{set: true, value: d} }

// UnmarshalYAML reads off, never, or a Go duration such as 90s, 15m, or 4h. Zero is refused rather
// than read as off, so a file never turns an alert off by a typo.
func (s *Setting) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("line %d: a threshold is a duration such as 15m, or off", node.Line)
	}
	raw = strings.ToLower(strings.TrimSpace(raw))
	switch raw {
	case "off", "never":
		*s = Off()
		return nil
	case "":
		return fmt.Errorf("line %d: a threshold is a duration such as 15m, or off", node.Line)
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration such as 15m, or off", node.Line, raw)
	}
	if d <= 0 {
		return fmt.Errorf("line %d: %q is not longer than zero, so write off to turn an alert off",
			node.Line, raw)
	}
	*s = After(d)
	return nil
}

// Thresholds are the settings an item is measured against, each absent until a file states it.
type Thresholds struct {
	// BlockedAfter is how long a run waits behind another run or a full worker before it shows as
	// blocked.
	BlockedAfter Setting `yaml:"blocked_after"`
	// AlertNoWorker is how long a run waits with no worker serving its queue before it alerts.
	AlertNoWorker Setting `yaml:"alert_no_worker"`
	// AlertBlocked is how long a run stays blocked before it alerts.
	AlertBlocked Setting `yaml:"alert_blocked"`
	// AlertWorkerLost is how long after a lost worker last reported its run alerts, when the run has
	// not been reclaimed by then.
	AlertWorkerLost Setting `yaml:"alert_worker_lost"`
	// AlertApproval is how long an approval waits before it alerts. It is off unless a file turns
	// it on, since an approval waiting is the gate working rather than something broken.
	AlertApproval Setting `yaml:"alert_approval"`
}

// overlay returns t with every setting o states replacing the one t has.
func (t Thresholds) overlay(o Thresholds) Thresholds {
	pick := func(base, over Setting) Setting {
		if over.set {
			return over
		}
		return base
	}
	return Thresholds{
		BlockedAfter:    pick(t.BlockedAfter, o.BlockedAfter),
		AlertNoWorker:   pick(t.AlertNoWorker, o.AlertNoWorker),
		AlertBlocked:    pick(t.AlertBlocked, o.AlertBlocked),
		AlertWorkerLost: pick(t.AlertWorkerLost, o.AlertWorkerLost),
		AlertApproval:   pick(t.AlertApproval, o.AlertApproval),
	}
}

// Config is a thresholds file: the organization's defaults, then optional overrides for one
// organization on the install, one queue, or one template.
type Config struct {
	// Defaults apply to every item.
	Defaults Thresholds `yaml:"defaults"`
	// Orgs override the defaults for the items an organization owns, keyed by organization id.
	Orgs map[string]Thresholds `yaml:"orgs"`
	// Queues override them for the runs on one queue, keyed by queue name. The default queue is
	// the empty name.
	Queues map[string]Thresholds `yaml:"queues"`
	// Templates override them for the runs a template launched, keyed by template id.
	Templates map[string]Thresholds `yaml:"templates"`
}

// LoadConfig reads a thresholds file. A file that cannot be read or parsed is an error rather than
// an empty configuration, so a typo never silently turns every alert back to its default.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// ParseConfig reads a thresholds document. An unknown key is refused, so a misspelled threshold is
// reported rather than ignored, and blocked_after cannot be off: it decides when a blocked run is
// shown at all, and alert_blocked is the setting that silences one.
func ParseConfig(raw []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	check := func(where string, t Thresholds) error {
		if t.BlockedAfter.set && t.BlockedAfter.value == 0 {
			return fmt.Errorf("%w: %s: blocked_after cannot be off, since it decides when a blocked "+
				"run is shown at all, so set alert_blocked: off to stop the alert instead", ErrConfig, where)
		}
		return nil
	}
	if err := check("defaults", cfg.Defaults); err != nil {
		return nil, err
	}
	for _, group := range []struct {
		// name labels the group in a refusal.
		name string
		// entries are the group's overrides.
		entries map[string]Thresholds
	}{{"orgs", cfg.Orgs}, {"queues", cfg.Queues}, {"templates", cfg.Templates}} {
		for key, t := range group.entries {
			if err := check(fmt.Sprintf("%s.%q", group.name, key), t); err != nil {
				return nil, err
			}
		}
	}
	return &cfg, nil
}

// Limits are the thresholds that apply to one item, every one resolved. A zero alert threshold
// never alerts.
type Limits struct {
	// BlockedAfter is how long a run waits before it shows as blocked.
	BlockedAfter time.Duration
	// AlertNoWorker is how long a run waits with no worker before it alerts.
	AlertNoWorker time.Duration
	// AlertBlocked is how long a run stays blocked before it alerts.
	AlertBlocked time.Duration
	// AlertWorkerLost is how long after a lost worker last reported its unreclaimed run alerts.
	AlertWorkerLost time.Duration
	// AlertApproval is how long an approval waits before it alerts.
	AlertApproval time.Duration
}

// Limits resolves the thresholds for an item owned by orgID, waiting on queue, launched from
// templateID: the built-in defaults, then the file's defaults, then the organization's, the
// queue's, and the template's overrides, each replacing only what it states. The template is the
// most specific, so it wins. leaseTTL is the lease period the worker-lost default is measured in. A
// nil Config resolves to the built-in defaults.
func (c *Config) Limits(orgID, queue, templateID string, leaseTTL time.Duration) Limits {
	t := Thresholds{
		BlockedAfter:    After(DefaultBlockedAfter),
		AlertNoWorker:   After(DefaultAlertNoWorker),
		AlertBlocked:    After(DefaultAlertBlocked),
		AlertWorkerLost: After(DefaultAlertWorkerLostLeases * leaseTTL),
		AlertApproval:   Off(),
	}
	if c != nil {
		t = t.overlay(c.Defaults)
		if o, ok := c.Orgs[orgID]; ok && orgID != "" {
			t = t.overlay(o)
		}
		if o, ok := c.Queues[queue]; ok {
			t = t.overlay(o)
		}
		if o, ok := c.Templates[templateID]; ok && templateID != "" {
			t = t.overlay(o)
		}
	}
	return Limits{
		BlockedAfter:    t.BlockedAfter.value,
		AlertNoWorker:   t.AlertNoWorker.value,
		AlertBlocked:    t.AlertBlocked.value,
		AlertWorkerLost: t.AlertWorkerLost.value,
		AlertApproval:   t.AlertApproval.value,
	}
}
