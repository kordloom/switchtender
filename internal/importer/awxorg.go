package importer

import "strings"

// orgKeySep separates an organization from a name in a lookup key. It cannot occur in either.
const orgKeySep = "\x00"

// awxIDs maps the objects a reference can name to the ids they were given. Two organizations may
// each hold an object of the same name, so an object is keyed by its organization and its name, and a
// reference that names an organization resolves only inside it.
//
// Keyed by name alone, the second organization's object replaced the first, and a job template in one
// organization was wired to the project and the inventory of another: a deploy written for one team's
// hosts ran against another team's.
type awxIDs map[string]string

// set records an object's id, reporting whether the same organization already held one of that name.
func (m awxIDs) set(org, name, id string) bool {
	key := org + orgKeySep + name
	_, dup := m[key]
	m[key] = id
	return dup
}

// get resolves a reference. One that names an organization matches only an object in it, or an
// object recorded with no organization at all. One that names none matches an object recorded with
// none, or the only object of that name, and never guesses between two.
func (m awxIDs) get(ref awxRef) (string, bool) {
	if ref.Name == "" {
		return "", false
	}
	if id, ok := m[ref.Org+orgKeySep+ref.Name]; ok {
		return id, true
	}
	if ref.Org != "" {
		id, ok := m[orgKeySep+ref.Name]
		return id, ok
	}
	match, found := "", 0
	for key, id := range m {
		if _, name, _ := strings.Cut(key, orgKeySep); name == ref.Name {
			match, found = id, found+1
		}
	}
	if found != 1 {
		return "", false
	}
	return match, true
}

// ambiguous reports whether a reference that names no organization matches objects in more than one.
func (m awxIDs) ambiguous(ref awxRef) bool {
	if ref.Org != "" || ref.Name == "" {
		return false
	}
	found := 0
	for key := range m {
		if _, name, _ := strings.Cut(key, orgKeySep); name == ref.Name {
			found++
		}
	}
	return found > 1
}

// unresolved words a reference that could not be wired, saying which of the two reasons it was.
func (m awxIDs) unresolved(ref awxRef) string {
	if m.ambiguous(ref) {
		return quoteName(ref.Name) + ", a name more than one organization uses, and the reference " +
			"does not say which, so it was not guessed at"
	}
	if ref.Org != "" {
		return quoteName(ref.Name) + " in organization " + quoteName(ref.Org)
	}
	return quoteName(ref.Name)
}

// orgQualifier returns the name an object is given here. The organization is prefixed only when the
// same name exists in more than one organization, so both arrive and can be told apart, while every
// other name stays exactly as it was.
func orgQualifier(objects ...[2]string) func(org, name string) string {
	orgs := map[string]map[string]bool{}
	for _, o := range objects {
		org, name := o[0], o[1]
		if orgs[name] == nil {
			orgs[name] = map[string]bool{}
		}
		orgs[name][org] = true
	}
	return func(org, name string) string {
		if org != "" && len(orgs[name]) > 1 {
			return org + "/" + name
		}
		return name
	}
}

// awxOrgNames lists the organization and name of each object, for orgQualifier.
func awxOrgNames[T any](objects []T, orgName func(T) (string, string)) [][2]string {
	out := make([][2]string, 0, len(objects))
	for _, o := range objects {
		org, name := orgName(o)
		out = append(out, [2]string{org, name})
	}
	return out
}

// awxJobs holds the export's job templates for the workflows that run them, resolved by organization
// and name the way every other reference is.
type awxJobs struct {
	// keys resolves a reference to the key its template is held under.
	keys awxIDs
	// byKey holds each template under its organization and name.
	byKey map[string]awxJobTemplate
}

// newAWXJobs indexes job templates by organization and name.
func newAWXJobs(list []awxJobTemplate) awxJobs {
	j := awxJobs{keys: awxIDs{}, byKey: make(map[string]awxJobTemplate, len(list))}
	for _, jt := range list {
		key := jt.Organization.Name + orgKeySep + jt.Name
		j.keys.set(jt.Organization.Name, jt.Name, key)
		j.byKey[key] = jt
	}
	return j
}

// get resolves a reference to the job template it names.
func (j awxJobs) get(ref awxRef) (awxJobTemplate, bool) {
	key, ok := j.keys.get(ref)
	if !ok {
		return awxJobTemplate{}, false
	}
	return j.byKey[key], true
}

// of returns the job template a reference names, or the zero template when it names none.
func (j awxJobs) of(ref awxRef) awxJobTemplate {
	jt, _ := j.get(ref)
	return jt
}
