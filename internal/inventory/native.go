package inventory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kordloom/switchtender/internal/util"
)

// The native engine resolves a self-contained static inventory document, INI or YAML or JSON, to
// exactly the hosts, groups, and variables ansible-inventory --list prints for it, without Ansible.
// The rule it serves: require Ansible only for what is genuinely Ansible, do the rest natively, and
// wherever the native code imitates Ansible, a conformance corpus run against every supported
// ansible-core release fails on any disagreement. A document only Ansible can resolve, such as a
// dynamic source's plugin configuration or a vault-encrypted value, is reported as needing Ansible
// rather than approximated; which engine resolves an inventory follows from its definition alone,
// never from whether Ansible happens to be installed.

// Resolution engines, as the evidence of a run names them.
const (
	// EngineNative is the native engine in this binary.
	EngineNative = "native"
	// EngineAnsible is Ansible's ansible-inventory, run as a separate program.
	EngineAnsible = "ansible"
)

// AnsibleInstallHint is the one line that installs Ansible's inventory and playbook commands on a
// server, quoted in every error that says Ansible is needed.
const AnsibleInstallHint = "pipx install ansible-core"

// TestedAnsibleCore lists the ansible-core releases, by minor version, the conformance corpus runs
// against in CI. The native engine's agreement with Ansible is proven for these and no others, and
// doctor warns when a server's ansible-core falls outside them.
var TestedAnsibleCore = []string{"2.16", "2.17", "2.18", "2.19", "2.20", "2.21"}

// TestedAnsibleCoreReleases lists the exact ansible-core release CI installs for each minor version
// in TestedAnsibleCore, pinned like every other tool the build installs, so a new patch release is
// tested only once somebody moves the pin.
var TestedAnsibleCoreReleases = []string{"2.16.19", "2.17.14", "2.18.19", "2.19.13", "2.20.9",
	"2.21.4"}

// NeedsAnsibleError reports that an inventory's definition can only be resolved by Ansible, and
// why.
type NeedsAnsibleError struct {
	// Reason says what in the definition needs Ansible, in a clause such as "it is an inventory
	// plugin configuration, which only Ansible runs".
	Reason string
}

// Error returns the reason.
func (e *NeedsAnsibleError) Error() string {
	return ErrNeedsAnsible.Error() + ": " + e.Reason
}

// Unwrap returns ErrNeedsAnsible, so errors.Is finds the sentinel.
func (e *NeedsAnsibleError) Unwrap() error { return ErrNeedsAnsible }

// needsAnsible returns a NeedsAnsibleError for reason.
func needsAnsible(reason string) error { return &NeedsAnsibleError{Reason: reason} }

// invalidf returns an ErrInvalidInventory carrying the formatted reason.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInventory, fmt.Sprintf(format, args...))
}

// NeedsAnsibleReason returns the reason an error says Ansible is needed, and false when it does
// not.
func NeedsAnsibleReason(err error) (string, bool) {
	var n *NeedsAnsibleError
	if errors.As(err, &n) {
		return n.Reason, true
	}
	return "", false
}

// ResolveNative resolves a static inventory document to the listing ansible-inventory --list prints
// for it: the same groups with the same direct hosts and child groups, and every host's variables
// with its group variables folded in by Ansible's precedence. It returns a NeedsAnsibleError for a
// document only Ansible can resolve and ErrInvalidInventory for one Ansible would not read whole.
func ResolveNative(content string) (*Listing, error) {
	d, err := nativeData(content)
	if err != nil {
		return nil, err
	}
	return d.listing()
}

// nativeData reads content into Ansible's inventory model the way ansible-inventory does for a file
// with no extension: the YAML plugin first, which reads JSON as well, and the INI plugin when the
// YAML plugin declines the file.
func nativeData(content string) (*invData, error) {
	if !utf8.ValidString(content) {
		return nil, needsAnsible("its content is not valid UTF-8")
	}
	if strings.HasPrefix(content, "\ufeff") {
		return nil, needsAnsible("its content starts with a byte order mark")
	}
	d := newInvData()
	doc, err := loadInventoryDocument(content)
	switch {
	case isNeedsAnsible(err):
		return nil, err
	case errors.Is(err, errAliasExpansion):
		// A document the YAML plugin loaded but whose aliases expand without bound. Ansible does not
		// bound this either, so it is refused here rather than handed to a tool that would hang.
		return nil, fmt.Errorf("%w: its YAML aliases expand to more than %d values, a size neither "+
			"this engine nor Ansible reads. Replace the repeated anchors with explicit values",
			ErrInvalidInventory, aliasNodeBudget(len(content)))
	case err != nil:
		// The YAML plugin declines a document that does not load, and the INI plugin gets its turn.
	default:
		switch err := parseYAMLInventory(d, doc); {
		case err == nil:
			if err := d.reconcile(); err != nil {
				return nil, invalidf("%s", err.Error())
			}
			return d, nil
		case errors.Is(err, errNotYAMLInventory):
			// Declined: not a mapping of groups. The INI plugin gets its turn.
		case isNeedsAnsible(err), errors.Is(err, ErrTooManyHosts):
			return nil, err
		case listForm(content):
			return nil, errListForm
		default:
			return nil, fmt.Errorf("%w. Ansible would go on to read the rest of the file as INI "+
				"on top of what it had read, so the result would be neither", err)
		}
	}
	d = newInvData()
	if err := parseINI(d, content); err != nil {
		if isNeedsAnsible(err) || errors.Is(err, ErrTooManyHosts) {
			return nil, err
		}
		if listForm(content) {
			return nil, errListForm
		}
		// The YAML reading is not quoted: a document that is not a mapping of groups is meant as
		// INI, and the INI reading is the one that says what is wrong with it.
		reason := strings.TrimPrefix(err.Error(), ErrInvalidInventory.Error()+": ")
		return nil, fmt.Errorf("%w: Ansible reads it as neither YAML nor INI. As INI, %s",
			ErrInvalidInventory, reason)
	}
	if err := d.reconcile(); err != nil {
		return nil, invalidf("%s", err.Error())
	}
	return d, nil
}

// errListForm refuses the JSON ansible-inventory --list prints, saying what to store instead.
var errListForm = fmt.Errorf("%w: this is the JSON ansible-inventory --list prints, which Ansible "+
	"does not read back as an inventory file; store the output of ansible-inventory --list --yaml "+
	"instead", ErrInvalidInventory)

// isNeedsAnsible reports whether err says Ansible is needed.
func isNeedsAnsible(err error) bool { return errors.Is(err, ErrNeedsAnsible) }

// listForm reports whether content looks like ansible-inventory --list output, whose _meta key and
// list-valued children no inventory plugin reads.
func listForm(content string) bool {
	trimmed := strings.TrimSpace(content)
	return strings.HasPrefix(trimmed, "{") && strings.Contains(trimmed, `"_meta"`)
}

// NativeHosts returns every host a static inventory document names, sorted by name, each with the
// variables set on the host itself, which are what AWX reads a host's own ansible_host from. It
// resolves the document exactly as ResolveNative does.
func NativeHosts(content string) ([]Host, error) {
	d, err := nativeData(content)
	if err != nil {
		return nil, err
	}
	out := make([]Host, 0, len(d.hosts))
	for _, name := range d.hostOrder {
		vars, err := d.ownVars(d.hosts[name])
		if err != nil {
			return nil, err
		}
		h := Host{Name: name}
		if len(vars) > 0 {
			h.Vars = vars
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// digestOf returns the sha256: digest of b.
func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Digest returns the digest a run's evidence records for this resolved inventory: SHA-256 over its
// canonical JSON, with groups and their hosts and children sorted and every secret-looking variable
// value masked, so two resolutions that agree have the same digest and the digest discloses no
// secret.
func (l *Listing) Digest() (string, error) {
	type canonicalGroup struct {
		// Hosts are the group's direct hosts, sorted.
		Hosts []string `json:"hosts"`
		// Children are the group's child groups, sorted.
		Children []string `json:"children"`
	}
	groups := make(map[string]canonicalGroup, len(l.groups))
	for name, g := range l.groups {
		cg := canonicalGroup{Hosts: slices.Sorted(slices.Values(g.Hosts)),
			Children: slices.Sorted(slices.Values(g.Children))}
		if cg.Hosts == nil {
			cg.Hosts = []string{}
		}
		if cg.Children == nil {
			cg.Children = []string{}
		}
		groups[name] = cg
	}
	vars := make(map[string]any, len(l.hostVars))
	for h, v := range l.hostVars {
		kept := make(map[string]any, len(v))
		for k, val := range v {
			if !reservedVars[k] {
				kept[k] = val
			}
		}
		if len(kept) > 0 {
			vars[h] = MaskSecrets(kept)
		}
	}
	body := map[string]any{"groups": groups, "hostvars": vars, "hosts": l.Hosts()}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return "", fmt.Errorf("%w: %w", ErrListing, err)
	}
	return digestOf(buf.Bytes()), nil
}

// MaskSecrets returns a copy of vars with the value of every secret-looking variable, at any depth,
// replaced by RedactedValue, and any secret assignment inside a string masked, the same rules the
// API applies when it serves an inventory.
func MaskSecrets(vars map[string]any) map[string]any {
	out := make(map[string]any, len(vars))
	for k, v := range vars {
		if util.SecretKey(k) {
			out[k] = RedactedValue
			continue
		}
		out[k] = maskValue(v)
	}
	return out
}

// maskValue masks secrets inside one value.
func maskValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return MaskSecrets(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = maskValue(item)
		}
		return out
	case string:
		masked, _ := util.RedactAssignments(t, RedactedValue)
		return masked
	}
	return v
}

// DigestInput is one input inventory a digest covers.
type DigestInput struct {
	// ID is the input inventory's id.
	ID string
	// Content is its content as resolved for the run.
	Content string
}

// InputDigest returns the digest a run's evidence records for the inputs a composed inventory read:
// SHA-256 over each input's id and its content with secret values masked, the form the API serves,
// in order, each framed by its length so no two input lists share a digest.
func InputDigest(inputs []DigestInput) string {
	h := sha256.New()
	for _, in := range inputs {
		masked := Redact(in.Content)
		fmt.Fprintf(h, "%d:%s\n%d:%s\n", len(in.ID), in.ID, len(masked), masked)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// sortedKeys returns m's keys sorted.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// ansibleCoreBanner matches the version an Ansible command's --version banner reports, as in
// "ansible-inventory [core 2.18.1]".
var ansibleCoreBanner = regexp.MustCompile(`\[core ([0-9]+\.[0-9]+[0-9A-Za-z.+-]*)\]`)

// ParseAnsibleCoreVersion returns the ansible-core version an Ansible command's --version output
// reports, or the empty string when it reports none.
func ParseAnsibleCoreVersion(banner string) string {
	if m := ansibleCoreBanner.FindStringSubmatch(banner); m != nil {
		return m[1]
	}
	return ""
}

// AnsibleCoreTested reports whether an ansible-core version falls within the releases the
// conformance corpus runs against.
func AnsibleCoreTested(version string) bool {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	return slices.Contains(TestedAnsibleCore, parts[0]+"."+parts[1])
}
