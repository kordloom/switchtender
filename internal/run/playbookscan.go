package run

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kordloom/switchtender/internal/util"
)

// taskKeywords are the Ansible task directives that are not the module being run. Anything else in
// a task mapping is the module, which is how Ansible itself reads a task.
var taskKeywords = map[string]bool{
	"name": true, "when": true, "loop": true, "with_items": true, "with_dict": true,
	"with_fileglob": true, "register": true, "tags": true, "become": true, "become_user": true,
	"become_method": true, "vars": true, "notify": true, "delegate_to": true, "run_once": true,
	"ignore_errors": true, "changed_when": true, "failed_when": true, "check_mode": true,
	"environment": true, "no_log": true, "until": true, "retries": true, "delay": true,
	"args": true, "loop_control": true, "any_errors_fatal": true, "throttle": true,
	"block": true, "rescue": true, "always": true, "listen": true, "connection": true,
	"local_action": true, "timeout": true, "poll": true, "async": true, "diff": true,
}

// destructiveModules are modules whose effect this product cannot undo, whatever arguments they
// carry. Matched against the last element of the module name, so the collection prefix does not
// have to be listed.
var destructiveModules = map[string]bool{
	"filesystem": true, "parted": true, "lvol": true, "lvg": true,
}

// dataBearingModules are modules whose removal destroys data rather than a definition, so state
// absent on one of them is permanent. A package or a service removed with state absent is a
// reinstall; a database or a filesystem path removed is gone.
var dataBearingModules = map[string]bool{
	"file": true, "mysql_db": true, "postgresql_db": true, "mongodb_db": true,
	"mssql_db": true, "influxdb_database": true, "elasticsearch_index": true,
	"s3_bucket": true, "gcs_bucket": true, "rds_instance": true, "ec2_instance": true,
	"lvol": true, "lvg": true, "user": true, "aws_s3": true,
}

// Bounds on how much one grade reads. A grade is computed on the submit path, so a repository must
// not be able to make it slow or large: past any of these the scan stops following, says so, and
// leaves the grade where the files already read put it.
const (
	// maxScanFileBytes is the largest single file the scan reads.
	maxScanFileBytes = 1 << 20
	// maxScanTotalBytes is how much the scan reads across every file.
	maxScanTotalBytes = 16 << 20
	// maxScanFiles is how many file reads one scan makes, the playbook included.
	maxScanFiles = 256
	// maxScanDepth is how deeply roles and includes may nest before the scan stops following them.
	maxScanDepth = 32
	// maxScanLabel is how much of a name read from a playbook the grade repeats.
	maxScanLabel = 80
)

// PlaybookSignals is what a playbook scan could see, and what it could not.
type PlaybookSignals struct {
	// Permanent lists the reasons the playbook holds something that cannot be undone.
	Permanent []string
	// Forced lists each play, block, task, role, or include that sets check_mode to anything but
	// literal true, naming the file and the task, which Ansible executes for real even under --check.
	Forced []string
	// Unread lists what the playbook pulls in that the scan could not read, each with why, so the
	// grade can say its own scope rather than implying it read everything that will run.
	Unread []string
	// Files counts the distinct files the scan read, the playbook included.
	Files int
	// Inputs lists the distinct files the scan read, sorted, the playbook included, which is what
	// the evidence of a dry run's scan names as examined.
	Inputs []string
	// Source says which version of the files was read, such as the commit, and is empty when the
	// files have only the one version.
	Source string
}

// ScanPlaybookIn reads the playbook at name inside fsys, and every role, role dependency, task
// include, and imported playbook it pulls in from the same tree, and reports what it can see about
// permanence. place names the tree in what the grade says, such as "the project".
//
// This exists because reversibility otherwise reads the command, and an Ansible run has none: the
// work is inside files, so a playbook that drops every database graded exactly like one that
// restarts a service. Real playbooks keep that work in roles, so reading the playbook alone left
// the grade blind exactly where the work is.
//
// A name is looked for everywhere the run could find it inside the tree, and every match is read,
// rather than the one match Ansible's search order would pick. Imitating that order is a second
// copy of Ansible's rules that can drift from the first, and a drift reads the wrong file and
// misses the right one. Reading every match can only find more, the one direction this may err in.
//
// The scan is honest about its reach. What it cannot read, a name only known at run time, a role
// that is not in the tree, a file outside it, is listed rather than guessed at, and leaves the
// grade where it was rather than lowering it. Work it could not see can never make a run look
// safer than it is: the only direction this moves a grade is toward permanent.
func ScanPlaybookIn(fsys fs.FS, name, place string) (PlaybookSignals, error) {
	var out PlaybookSignals
	s := &playbookScan{fsys: fsys, place: place, out: &out, files: map[string]bool{},
		visited: map[string]bool{}, roles: map[string]bool{}, listed: map[string]bool{}}
	content, err := s.read(name)
	if err != nil {
		return PlaybookSignals{}, fmt.Errorf("read playbook: %w", err)
	}
	var plays []map[string]any
	if err := yaml.Unmarshal(content, &plays); err != nil {
		return PlaybookSignals{}, fmt.Errorf("parse playbook: %w", err)
	}
	s.loadConfig()
	s.scanPlays(name, plays, 0)
	for file := range s.files {
		out.Inputs = append(out.Inputs, file)
	}
	sort.Strings(out.Inputs)
	return out, nil
}

// playbookScan is one walk over a playbook and what it pulls in.
type playbookScan struct {
	// fsys holds the playbook and everything the scan may read. Nothing outside it is read.
	fsys fs.FS
	// place names fsys in what the grade says.
	place string
	// out collects what the walk finds.
	out *PlaybookSignals
	// files records each distinct file read, for the count the grade reports.
	files map[string]bool
	// visited records each file walked in each context, so a file pulled in twice from the same
	// place is walked once and a cycle of includes ends.
	visited map[string]bool
	// roles records the role entry points already followed.
	roles map[string]bool
	// listed records the findings and unread entries already reported, so a missing role named ten
	// times is reported once.
	listed map[string]bool
	// reads counts the file reads made, against maxScanFiles.
	reads int
	// bytes counts what the walk has read, against maxScanTotalBytes.
	bytes int
	// rolesPath is the role search path the tree's ansible.cfg adds.
	rolesPath []string
	// collectionsPath is the collection search path the tree's ansible.cfg adds.
	collectionsPath []string
}

// scanContext is where a task list sits, which decides how the names it pulls in resolve.
type scanContext struct {
	// file is the file holding the tasks.
	file string
	// basedir is the directory of the playbook the tasks run under, which playbook-relative names
	// resolve against.
	basedir string
	// role is the directory of the role the tasks belong to, empty outside a role.
	role string
	// collections are the collections the play names, which a short role name may come from.
	collections []string
	// depth is how many roles and includes deep the tasks are.
	depth int
}

// read returns one file's content and counts it against the scan's bounds.
func (s *playbookScan) read(name string) ([]byte, error) {
	if s.reads >= maxScanFiles || s.bytes >= maxScanTotalBytes {
		return nil, errScanLimit
	}
	s.reads++
	f, err := s.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	content, err := io.ReadAll(io.LimitReader(f, maxScanFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxScanFileBytes {
		return nil, errScanTooLarge
	}
	s.bytes += len(content)
	if !s.files[name] {
		s.files[name] = true
		s.out.Files++
	}
	return content, nil
}

// why says why a file could not be read, in the words a grade uses.
func (s *playbookScan) why(err error) string {
	switch {
	case errors.Is(err, errScanLimit):
		return fmt.Sprintf("past the %d files or %d MiB one grade reads", maxScanFiles,
			maxScanTotalBytes>>20)
	case errors.Is(err, errScanTooLarge):
		return fmt.Sprintf("larger than the %d MiB one file may be", maxScanFileBytes>>20)
	case errors.Is(err, fs.ErrNotExist):
		return "not in " + s.place
	default:
		return "could not be read"
	}
}

// skip lists something the playbook pulls in that the scan could not read, and why.
func (s *playbookScan) skip(what, why string) {
	entry := what + " (" + why + ")"
	if s.listed[entry] {
		return
	}
	s.listed[entry] = true
	s.out.Unread = append(s.out.Unread, entry)
}

// permanent records a finding once, however many times its file is walked.
func (s *playbookScan) permanent(finding string) {
	if s.listed[finding] {
		return
	}
	s.listed[finding] = true
	s.out.Permanent = append(s.out.Permanent, finding)
}

// isDir reports whether name is a directory in the tree.
func (s *playbookScan) isDir(name string) bool {
	info, err := fs.Stat(s.fsys, name)
	return err == nil && info.IsDir()
}

// isFile reports whether name is a regular file in the tree.
func (s *playbookScan) isFile(name string) bool {
	info, err := fs.Stat(s.fsys, name)
	return err == nil && info.Mode().IsRegular()
}

// locate returns the candidates that exist, as directories when wantDir is set and as files
// otherwise, and lists what as unread when none does. A name only known at run time, an absolute
// name, and one nested too deeply are listed without looking.
func (s *playbookScan) locate(what, name string, depth int, candidates []string, wantDir bool) []string {
	switch {
	case templated(name):
		s.skip(what, "named at run time")
		return nil
	case path.IsAbs(name) || strings.HasPrefix(name, "~"):
		s.skip(what, "outside "+s.place)
		return nil
	case depth > maxScanDepth:
		s.skip(what, fmt.Sprintf("nested more than %d deep", maxScanDepth))
		return nil
	}
	var found []string
	inside := false
	tried := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		if !fs.ValidPath(c) || tried[c] {
			continue
		}
		tried[c] = true
		inside = true
		if (wantDir && s.isDir(c)) || (!wantDir && s.isFile(c)) {
			found = append(found, c)
		}
	}
	switch {
	case len(found) > 0:
	case inside:
		s.skip(what, "not in "+s.place)
	default:
		s.skip(what, "outside "+s.place)
	}
	return found
}

// scanPlays walks the plays of one playbook file.
func (s *playbookScan) scanPlays(file string, plays []map[string]any, depth int) {
	basedir := path.Dir(file)
	for _, play := range plays {
		s.checkMode(file, playLabel(play), play)
		for _, key := range sortedKeys(play) {
			// An imported playbook is a play-level entry naming another playbook, resolved against
			// the directory of the one importing it.
			if how := shortName(key); how == "import_playbook" || how == "include" {
				s.followPlaybook(basedir, how, play[key], depth+1)
			}
		}
		ctx := scanContext{file: file, basedir: basedir, collections: stringList(play["collections"]),
			depth: depth}
		for _, entry := range asList(play["roles"]) {
			if args, ok := entry.(map[string]any); ok {
				s.checkMode(file, label("role", roleName(entry)), args)
			}
			s.followRole(ctx, "role", roleName(entry), "", "")
		}
		for _, key := range []string{"pre_tasks", "tasks", "post_tasks", "handlers"} {
			s.scanTaskList(ctx, play[key])
		}
	}
}

// followPlaybook reads a playbook an import_playbook names, from beside the one importing it.
func (s *playbookScan) followPlaybook(basedir, how string, value any, depth int) {
	name := includeTarget(value)
	if name == "" {
		return
	}
	what := label(how, name)
	for _, file := range s.locate(what, name, depth, []string{path.Join(basedir, name)}, false) {
		if s.visited[file] {
			continue
		}
		s.visited[file] = true
		content, err := s.read(file)
		if err != nil {
			s.skip(what, s.why(err))
			continue
		}
		var plays []map[string]any
		if err := yaml.Unmarshal(content, &plays); err != nil {
			s.skip(what, "not a playbook")
			continue
		}
		s.scanPlays(file, plays, depth)
	}
}

// followRole reads a role's tasks, handlers, and dependencies from every place the run could find
// it. tasksFrom and handlersFrom name the files to read when an include_role asks for other than
// main.
func (s *playbookScan) followRole(ctx scanContext, how, name, tasksFrom, handlersFrom string) {
	if name == "" {
		return
	}
	what := label(how, name)
	if templated(tasksFrom) || templated(handlersFrom) {
		s.skip(what, "its tasks are named at run time")
		return
	}
	for _, dir := range s.locate(what, name, ctx.depth+1, s.roleCandidates(ctx, name), true) {
		key := dir + "\x00" + tasksFrom + "\x00" + handlersFrom
		if s.roles[key] {
			continue
		}
		s.roles[key] = true
		inner := scanContext{basedir: ctx.basedir, role: dir, collections: ctx.collections,
			depth: ctx.depth + 1}
		s.scanRoleFile(inner, what, path.Join(dir, "tasks"), tasksFrom)
		s.scanRoleFile(inner, what, path.Join(dir, "handlers"), handlersFrom)
		s.followDependencies(inner, what, dir)
	}
}

// roleCandidates lists every path a role name could resolve to inside the tree: inside a collection
// when the name is qualified or the play names collections, under the playbook's roles directory,
// on the configured and installed role paths, beside the role that pulls it in, and as a path from
// the playbook or from the root the run starts in.
func (s *playbookScan) roleCandidates(ctx scanContext, name string) []string {
	var out []string
	roots := s.collectionRoots(ctx)
	if parts := strings.Split(name, "."); len(parts) >= 3 && !strings.Contains(name, "/") {
		for _, root := range roots {
			out = append(out, path.Join(root, "ansible_collections", parts[0], parts[1], "roles",
				strings.Join(parts[2:], ".")))
		}
	}
	if !strings.ContainsAny(name, "./") {
		for _, coll := range ctx.collections {
			ns, c, ok := strings.Cut(coll, ".")
			if !ok || strings.Contains(c, ".") {
				continue
			}
			for _, root := range roots {
				out = append(out, path.Join(root, "ansible_collections", ns, c, "roles", name))
			}
		}
	}
	out = append(out, path.Join(ctx.basedir, "roles", name))
	for _, p := range s.rolesPath {
		out = append(out, path.Join(p, name))
	}
	out = append(out, path.Join(".galaxy", "roles", name))
	if ctx.role != "" {
		out = append(out, path.Join(path.Dir(ctx.role), name))
	}
	return append(out, path.Join(ctx.basedir, name), name)
}

// collectionRoots lists the directories inside the tree that may hold an ansible_collections
// directory: where a project's dependencies are installed, beside the playbook, at the root, and
// wherever ansible.cfg adds.
func (s *playbookScan) collectionRoots(ctx scanContext) []string {
	roots := []string{path.Join(".galaxy", "collections"), path.Join(ctx.basedir, "collections"),
		"collections"}
	return append(roots, s.collectionsPath...)
}

// scanRoleFile reads one of a role's task or handler files, main unless from names another. A role
// without a main file is ordinary, since a role may carry only handlers or only dependencies, so a
// missing file is listed only when it was asked for by name.
func (s *playbookScan) scanRoleFile(ctx scanContext, what, dir, from string) {
	asked := from != ""
	if !asked {
		from = "main"
	}
	found := false
	for _, name := range []string{from, from + ".yml", from + ".yaml"} {
		file := path.Join(dir, name)
		if !fs.ValidPath(file) || !s.isFile(file) {
			continue
		}
		found = true
		s.scanTaskFile(ctx, what, file)
	}
	if asked && !found {
		s.skip(what+" "+path.Base(dir)+" "+strconv.Quote(util.Clip(from, maxScanLabel)),
			"not in "+s.place)
	}
}

// followDependencies follows the roles a role's meta/main.yml depends on, which Ansible runs before
// the role itself whether or not the playbook names them.
func (s *playbookScan) followDependencies(ctx scanContext, what, dir string) {
	for _, name := range []string{"main", "main.yml", "main.yaml"} {
		file := path.Join(dir, "meta", name)
		if !fs.ValidPath(file) || !s.isFile(file) || s.visited[file] {
			continue
		}
		s.visited[file] = true
		content, err := s.read(file)
		if err != nil {
			s.skip(what+" dependencies", s.why(err))
			continue
		}
		var meta map[string]any
		if err := yaml.Unmarshal(content, &meta); err != nil {
			s.skip(what+" dependencies", "not valid role metadata")
			continue
		}
		for _, dep := range asList(meta["dependencies"]) {
			if args, ok := dep.(map[string]any); ok {
				s.checkMode(file, label("role dependency", roleName(dep)), args)
			}
			s.followRole(ctx, "role", roleName(dep), "", "")
		}
	}
}

// followTasks reads the task file an include_tasks or import_tasks names, from every place the run
// could find it: beside the file doing the including, in the role's tasks, and beside the playbook.
func (s *playbookScan) followTasks(ctx scanContext, how string, value any) {
	name := includeTarget(value)
	if name == "" {
		return
	}
	what := label(how, name)
	candidates := []string{path.Join(path.Dir(ctx.file), name)}
	if ctx.role != "" {
		candidates = append(candidates, path.Join(ctx.role, "tasks", name), path.Join(ctx.role, name))
	}
	candidates = append(candidates, path.Join(ctx.basedir, name))
	inner := ctx
	inner.depth++
	for _, file := range s.locate(what, name, inner.depth, candidates, false) {
		s.scanTaskFile(inner, what, file)
	}
}

// scanTaskFile reads a file of tasks and walks it. A file is walked once per role and playbook it
// is reached from, since the names it pulls in resolve against those.
func (s *playbookScan) scanTaskFile(ctx scanContext, what, file string) {
	key := file + "\x00" + ctx.role + "\x00" + ctx.basedir
	if s.visited[key] {
		return
	}
	s.visited[key] = true
	content, err := s.read(file)
	if err != nil {
		s.skip(what, s.why(err))
		return
	}
	var tasks []any
	if err := yaml.Unmarshal(content, &tasks); err != nil {
		s.skip(what, "not a list of tasks")
		return
	}
	ctx.file = file
	s.scanTaskList(ctx, tasks)
}

// scanTaskList walks a list of tasks, descending into blocks.
func (s *playbookScan) scanTaskList(ctx scanContext, raw any) {
	for _, item := range asList(raw) {
		task, ok := item.(map[string]any)
		if !ok {
			continue
		}
		s.checkMode(ctx.file, taskLabel(task), task)
		// A block holds more tasks, and its rescue and always paths run real work too.
		for _, nested := range []string{"block", "rescue", "always"} {
			if inner, ok := task[nested]; ok {
				s.scanTaskList(ctx, inner)
			}
		}
		s.scanTask(ctx, task)
	}
}

// scanTask grades one task mapping and follows what it pulls in.
func (s *playbookScan) scanTask(ctx scanContext, task map[string]any) {
	where := util.Clip(util.SafeText(ctx.file), 2*maxScanLabel)
	for _, key := range sortedKeys(task) {
		if taskKeywords[key] {
			continue
		}
		value := task[key]
		module := shortName(key)
		switch module {
		case "include_tasks", "import_tasks", "include":
			s.checkApply(ctx.file, task, value)
			s.followTasks(ctx, module, value)
			continue
		case "include_role", "import_role":
			s.checkApply(ctx.file, task, value)
			args := moduleArgs(value)
			s.followRole(ctx, module, stringArg(args, "name"), stringArg(args, "tasks_from"),
				stringArg(args, "handlers_from"))
			continue
		case "command", "shell", "raw", "script":
			// The module's own text is a command, so the shared command scanner judges it. A third
			// private copy of that judgment is how the graders drifted apart the first time.
			if text := rawParams(value); text != "" {
				for _, finding := range permanentCommandFindings(strings.ToLower(text)) {
					s.permanent("a " + module + " task in " + where + ": " + finding)
				}
			}
			continue
		}
		if destructiveModules[module] {
			s.permanent("a task in " + where + " uses the " + module +
				" module, whose effect cannot be undone from here")
			continue
		}
		if dataBearingModules[module] && argState(value) == "absent" {
			s.permanent("a task in " + where + " removes a " + module +
				" with state absent, which destroys what it held")
		}
	}
}

// checkMode records a play, block, task, role entry, or include that sets check_mode to anything
// but literal true. Ansible runs such work for real even under --check, so a dry run of a playbook
// holding one is not a preview. A value that is templated, null, or anything Ansible would not read
// as true is recorded too: what it evaluates to is decided at run time, after the gate has passed
// the run, so it is taken as the value that changes hosts.
func (s *playbookScan) checkMode(file, what string, holder map[string]any) {
	value, ok := holder["check_mode"]
	if !ok || checkModeTrue(value) {
		return
	}
	finding := util.Clip(util.SafeText(file), 2*maxScanLabel) + ": " + what + " sets check_mode to " +
		checkModeText(value)
	if s.listed[finding] {
		return
	}
	s.listed[finding] = true
	s.out.Forced = append(s.out.Forced, finding)
}

// checkApply records an include whose apply keyword sets check_mode on everything it pulls in. A
// keyword on a dynamic include binds the include itself, and apply is how it reaches the tasks
// included, so both are read.
func (s *playbookScan) checkApply(file string, task map[string]any, value any) {
	apply, ok := moduleArgs(value)["apply"].(map[string]any)
	if !ok {
		return
	}
	s.checkMode(file, "the apply of "+taskLabel(task), apply)
}

// checkModeTrue reports whether a check_mode value is one Ansible reads as true without evaluating
// anything: the boolean, or one of the spellings its boolean conversion accepts.
func checkModeTrue(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case int:
		return v == 1
	case float64:
		return v == 1
	case string:
		if templated(v) {
			return false
		}
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "yes", "on", "y", "t", "1":
			return true
		}
	}
	return false
}

// checkModeText renders a check_mode value the way a finding repeats it.
func checkModeText(value any) string {
	switch v := value.(type) {
	case nil:
		return "nothing, which is not true"
	case bool:
		return strconv.FormatBool(v)
	case string:
		text := strconv.Quote(util.Clip(util.SafeText(v), maxScanLabel))
		if templated(v) {
			return text + ", which is only decided at run time"
		}
		return text
	default:
		return strconv.Quote(util.Clip(util.SafeText(fmt.Sprint(v)), maxScanLabel))
	}
}

// playLabel names a play as a finding repeats it.
func playLabel(play map[string]any) string {
	for _, key := range []string{"import_playbook", "ansible.builtin.import_playbook"} {
		if target := includeTarget(play[key]); target != "" {
			return label("import_playbook", target)
		}
	}
	if name := stringArg(play, "name"); name != "" {
		return label("play", name)
	}
	return "an unnamed play"
}

// taskLabel names a task or block as a finding repeats it: by its name, or by the module it runs
// when it has none, so an approver can find it in the file.
func taskLabel(task map[string]any) string {
	kind := "task"
	if _, ok := task["block"]; ok {
		kind = "block"
	}
	if name := stringArg(task, "name"); name != "" {
		return label(kind, name)
	}
	if kind == "task" {
		for _, key := range sortedKeys(task) {
			if !taskKeywords[key] {
				return "an unnamed " + strconv.Quote(util.Clip(key, maxScanLabel)) + " task"
			}
		}
	}
	return "an unnamed " + kind
}

// loadConfig reads the role and collection search paths from the ansible.cfg at the root of the
// tree. A run starts at the root, so that file is the configuration it uses. Only paths inside the
// tree are kept: an absolute path names a directory on some machine, not one in what was read.
func (s *playbookScan) loadConfig() {
	f, err := s.fsys.Open("ansible.cfg")
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	content, err := io.ReadAll(io.LimitReader(f, maxScanFileBytes))
	if err != nil {
		return
	}
	section := ""
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		cut := strings.IndexAny(line, "=:")
		if section != "defaults" || cut < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:cut]))
		value := line[cut+1:]
		for _, marker := range []string{" #", " ;", "\t#", "\t;"} {
			if i := strings.Index(value, marker); i >= 0 {
				value = value[:i]
			}
		}
		switch key {
		case "roles_path":
			s.rolesPath = append(s.rolesPath, treePaths(value)...)
		case "collections_path", "collections_paths":
			s.collectionsPath = append(s.collectionsPath, treePaths(value)...)
		}
	}
}

// treePaths splits a colon-separated search path from ansible.cfg and keeps the entries inside the
// tree, cleaned.
func treePaths(value string) []string {
	var out []string
	for _, entry := range strings.Split(value, ":") {
		entry = strings.TrimSpace(entry)
		if entry == "" || path.IsAbs(entry) || strings.ContainsAny(entry, "~$") || templated(entry) {
			continue
		}
		if cleaned := path.Clean(entry); fs.ValidPath(cleaned) {
			out = append(out, cleaned)
		}
	}
	return out
}

// label names something a playbook pulls in, as the grade repeats it.
func label(how, name string) string {
	return how + " " + strconv.Quote(util.Clip(name, maxScanLabel))
}

// templated reports whether a value is a Jinja expression, whose result is only known at run time.
func templated(value string) bool {
	return strings.Contains(value, "{{") || strings.Contains(value, "{%")
}

// shortName returns the last element of a possibly collection-qualified module or keyword name.
func shortName(key string) string {
	if i := strings.LastIndex(key, "."); i >= 0 {
		return key[i+1:]
	}
	return key
}

// sortedKeys returns a mapping's keys in order, so what a scan reports does not change between two
// reads of the same file.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// asList returns a YAML sequence, nil for anything else.
func asList(raw any) []any {
	list, _ := raw.([]any)
	return list
}

// stringList returns the strings in a YAML sequence.
func stringList(raw any) []string {
	var out []string
	for _, item := range asList(raw) {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// roleName returns the role a roles entry or a dependency names: a bare string, or the role or name
// key of a mapping.
func roleName(entry any) string {
	switch v := entry.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		return util.FirstNonEmpty(stringArg(v, "role"), stringArg(v, "name"))
	}
	return ""
}

// includeTarget returns the file an include or import names, whether written inline, as file=, or
// as a mapping.
func includeTarget(value any) string {
	switch v := value.(type) {
	case string:
		text := strings.TrimSpace(v)
		if templated(text) {
			return text
		}
		args, first := splitFreeform(text)
		return util.FirstNonEmpty(stringArg(args, "file"), first)
	case map[string]any:
		return util.FirstNonEmpty(stringArg(v, "file"), stringArg(v, "_raw_params"))
	}
	return ""
}

// moduleArgs returns a module's arguments as a mapping, whether written as one or inline as
// key=value pairs.
func moduleArgs(value any) map[string]any {
	switch v := value.(type) {
	case map[string]any:
		return v
	case string:
		args, _ := splitFreeform(v)
		return args
	}
	return nil
}

// splitFreeform splits Ansible's inline key=value form into its arguments and the first word that
// is not one.
func splitFreeform(text string) (map[string]any, string) {
	args := map[string]any{}
	first := ""
	for _, word := range strings.Fields(text) {
		if k, v, ok := strings.Cut(word, "="); ok && k != "" && !strings.ContainsAny(k, "/.") {
			args[k] = v
			continue
		}
		if first == "" {
			first = word
		}
	}
	return args, first
}

// stringArg returns a mapping's string value for key, trimmed, empty when absent or not a string.
func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}

// rawParams returns a command module's text whether it was written inline or as a mapping.
func rawParams(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case map[string]any:
		if raw, ok := v["cmd"].(string); ok {
			return raw
		}
		if raw, ok := v["_raw_params"].(string); ok {
			return raw
		}
	}
	return ""
}

// argState returns a module's state argument, empty when it has none.
func argState(value any) string {
	args, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	state, _ := args["state"].(string)
	return strings.ToLower(state)
}
