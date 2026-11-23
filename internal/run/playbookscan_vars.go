package run

import (
	"bytes"
	"io/fs"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// vaultHeader starts a file ansible-vault encrypted whole.
const vaultHeader = "$ANSIBLE_VAULT"

// varFileName reports whether Ansible loads name as a variable file when it sits in a vars
// directory: YAML, JSON, or a file with no extension.
func varFileName(name string) bool {
	switch path.Ext(name) {
	case "", ".yml", ".yaml", ".json":
		return true
	}
	return false
}

// inVarsTree reports whether name sits under a group_vars or host_vars directory, which Ansible
// loads beside an inventory or a playbook with no task naming it.
func inVarsTree(name string) bool {
	for _, part := range strings.Split(path.Dir(name), "/") {
		if part == "group_vars" || part == "host_vars" {
			return true
		}
	}
	return false
}

// inTemplatesDir reports whether name sits under a directory named templates, where a template
// task's source is looked up.
func inTemplatesDir(name string) bool {
	return strings.Contains("/"+path.Dir(name)+"/", "/templates/")
}

// judgeVarDir judges every variable file under dir, the way a role's defaults and vars, or an
// include_vars dir, are loaded. A directory that is not there loads nothing.
func (s *playbookScan) judgeVarDir(dir string) {
	if !s.isDir(dir) {
		return
	}
	_ = fs.WalkDir(s.fsys, dir, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			s.skip(label("variable directory", name), "could not be listed")
			return nil
		}
		if !d.IsDir() && varFileName(name) {
			s.judgeVarFile(name)
		}
		return nil
	})
}

// judgeVarFile gives every string in one variable file to controllerExec and every name it defines
// to ConnectionVar.
//
// A variable's value is templated on the controller when a task uses it, so a lookup in a role's
// defaults, a vars_files entry, or group_vars runs under --check the same as one written in the
// task. The scan read only tasks and play vars, so a playbook whose tasks named a variable holding a
// pipe lookup was classified change free. A file that cannot be read as plain data, an encrypted
// one included, could hold either, so it is listed as unread and the scan is incomplete. A file the
// walk already read, as tasks or as variables, is not judged again.
func (s *playbookScan) judgeVarFile(name string) {
	if s.files[name] {
		return
	}
	what := label("variable file", name)
	content, err := s.read(name)
	if err != nil {
		s.skip(what, s.why(err))
		return
	}
	if bytes.HasPrefix(bytes.TrimSpace(content), []byte(vaultHeader)) {
		s.skip(what, "vault encrypted, so the scan cannot read the values Ansible templates")
		return
	}
	var node yaml.Node
	if yaml.Unmarshal(content, &node) != nil {
		s.skip(what, "does not parse as plain data")
		return
	}
	if holdsVaultValue(&node) {
		s.skip(what, "holds a vault-encrypted value, which the scan cannot read")
		return
	}
	var doc any
	if node.Decode(&doc) != nil {
		s.skip(what, "does not parse as plain data")
		return
	}
	s.controllerExec(name, "a variable file", doc)
	s.connectionVars(name, "a variable file", doc)
}

// holdsVaultValue reports whether any node under n carries the !vault tag, a value Ansible
// decrypts and then templates like any other.
func holdsVaultValue(n *yaml.Node) bool {
	if n.Tag == "!vault" {
		return true
	}
	for _, c := range n.Content {
		if holdsVaultValue(c) {
			return true
		}
	}
	return false
}

// judgeTemplate runs the lookup matcher over the raw text of one template source. Ansible renders
// the whole file on the controller, so a lookup anywhere in it runs under --check. An encrypted
// template could hold one, so it is listed as unread.
func (s *playbookScan) judgeTemplate(name string) {
	if s.files[name] {
		return
	}
	content, err := s.read(name)
	if err != nil {
		s.skip(label("template", name), s.why(err))
		return
	}
	if bytes.HasPrefix(bytes.TrimSpace(content), []byte(vaultHeader)) {
		s.skip(label("template", name), "vault encrypted, so the scan cannot read it")
		return
	}
	if why := unclearedLookup(string(content)); why != "" {
		s.forceControllerExec(name, "a template", why)
	}
}

// followVarsFiles judges each file a play's vars_files names. An entry that is a list loads the
// first of its files that exists, so every one of them is judged.
func (s *playbookScan) followVarsFiles(basedir string, value any, depth int) {
	for _, entry := range asList(value) {
		names := stringList(entry)
		if name, ok := entry.(string); ok {
			names = []string{name}
		}
		for _, name := range names {
			for _, file := range s.locate(label("vars_files entry", name), name, depth,
				[]string{path.Join(basedir, name)}, false) {
				s.judgeVarFile(file)
			}
		}
	}
}

// followIncludeVars judges what an include_vars task loads: a file, looked up in the role's vars,
// beside the task file, and from the playbook, or a whole directory.
func (s *playbookScan) followIncludeVars(ctx scanContext, value any) {
	if dir := stringArg(moduleArgs(value), "dir"); dir != "" {
		var cands []string
		if ctx.role != "" {
			cands = append(cands, path.Join(ctx.role, "vars", dir), path.Join(ctx.role, dir))
		}
		cands = append(cands, path.Join(path.Dir(ctx.file), dir), path.Join(ctx.basedir, dir))
		for _, d := range s.locate(label("include_vars dir", dir), dir, ctx.depth, cands, true) {
			s.judgeVarDir(d)
		}
		return
	}
	name := includeTarget(value)
	if name == "" {
		return
	}
	var cands []string
	if ctx.role != "" {
		cands = append(cands, path.Join(ctx.role, "vars", name), path.Join(ctx.role, name))
	}
	cands = append(cands, path.Join(path.Dir(ctx.file), "vars", name), path.Join(path.Dir(ctx.file), name),
		path.Join(ctx.basedir, "vars", name), path.Join(ctx.basedir, name))
	for _, file := range s.locate(label("include_vars file", name), name, ctx.depth, cands, false) {
		s.judgeVarFile(file)
	}
}

// followTemplate judges the source a template task renders, looked up in the role's templates,
// beside the task file, and from the playbook.
func (s *playbookScan) followTemplate(ctx scanContext, value any) {
	src := stringArg(moduleArgs(value), "src")
	if src == "" {
		return
	}
	var cands []string
	if ctx.role != "" {
		cands = append(cands, path.Join(ctx.role, "templates", src), path.Join(ctx.role, src))
	}
	cands = append(cands, path.Join(path.Dir(ctx.file), "templates", src), path.Join(path.Dir(ctx.file), src),
		path.Join(ctx.basedir, "templates", src), path.Join(ctx.basedir, src))
	for _, file := range s.locate(label("template", src), src, ctx.depth, cands, false) {
		s.judgeTemplate(file)
	}
}
