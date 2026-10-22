package witness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A lapsed install's record outlives its license. For LapseGrace after the license term ends the
// witness still countersigns its pushes. For LapseReadOnlyMonths after that it countersigns nothing
// more, and its statement and findings stay readable, so an auditor asking about the covered period
// still gets an answer. Then the sweep deletes the record's directory under installs/ and nothing
// else. A newer license that arrives while the record is still held reopens that record and its
// memory, and only an install whose record was deleted starts a fresh memory.
const (
	// LapseReadOnlyMonths is how many months a lapsed install's record stays readable once its grace
	// has passed. The sweep deletes the record after them.
	LapseReadOnlyMonths = 12
	// retiredSuffix names a record directory whose deletion has begun. The rename to it is the one
	// step that decides a deletion, so a crash partway through removing the files can never leave a
	// directory that loads, or that a fresh enrollment of the same key would find, as the old memory.
	retiredSuffix = ".retired"
)

// deleteAfter returns when the record of an install whose license term ends at expires is deleted:
// LapseReadOnlyMonths after its grace ends. It returns the zero time, never, for no term.
func deleteAfter(expires time.Time) time.Time {
	if expires.IsZero() {
		return time.Time{}
	}
	return expires.Add(LapseGrace).AddDate(0, LapseReadOnlyMonths, 0)
}

// retentionEnded reports whether the install's record has outlived its read-only year at now.
// Callers hold the install's lock or installsMu.
func (in *install) retentionEnded(now time.Time) bool {
	due := deleteAfter(in.expires)
	return !due.IsZero() && now.After(due)
}

// retireIfDue deletes the install's record when its read-only year has ended, and reports whether
// the record is gone. The look under the install's lock alone keeps a sweep over records that are
// not due from taking the service's write lock. The deletion then takes both locks in their order
// and looks again, because a newer license may have reopened the record in between.
func (s *Service) retireIfDue(in *install, now time.Time) bool {
	in.mu.Lock()
	due := in.retentionEnded(now)
	in.mu.Unlock()
	if !due {
		return false
	}
	s.installsMu.Lock()
	defer s.installsMu.Unlock()
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.gone || !in.retentionEnded(now) {
		return in.gone
	}
	if err := s.deleteRecord(in); err != nil {
		s.log.Error("witness: delete the record of " + in.rec.KeyID + ": " + err.Error())
		return false
	}
	return true
}

// deleteRecord removes an install's record from the service and its directory from installs/,
// logging the producer key id and writing nothing else. Callers hold installsMu for writing and the
// install's lock. The directory is renamed first, which decides the deletion, and removed second;
// what a crash leaves of the second step is finished at the next start.
func (s *Service) deleteRecord(in *install) error {
	root := filepath.Join(s.dir, installsDir)
	if _, ok := recordName("sha256:" + in.name); !ok || in.dir != filepath.Join(root, in.name) {
		return fmt.Errorf("the record %s is not a directory of %s", in.dir, root)
	}
	retired := in.dir + retiredSuffix
	// A retired directory under this name is left from an earlier deletion of the same key that the
	// disk did not let finish, so it holds nothing anyone may read.
	if err := os.RemoveAll(retired); err != nil {
		return err
	}
	if err := os.Rename(in.dir, retired); err != nil {
		return err
	}
	delete(s.installs, in.name)
	in.gone = true
	s.log.Info(fmt.Sprintf("witness: deleted the record of producer key %s, %d %s after its license "+
		"term ended %s and its grace passed", in.rec.KeyID, LapseReadOnlyMonths,
		plural(LapseReadOnlyMonths, "month", "months"), stamp(in.expires)))
	if err := os.RemoveAll(retired); err != nil {
		s.log.Error("witness: finish deleting the record of " + in.rec.KeyID + ", left for the " +
			"next start: " + err.Error())
	}
	return nil
}

// finishRetired removes a record directory whose deletion began in an earlier process and reports
// whether name was one. It is never read as a record either way.
func (s *Service) finishRetired(root, name string) bool {
	if !strings.HasSuffix(name, retiredSuffix) {
		return false
	}
	if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
		s.log.Error("witness: finish deleting the record " + strings.TrimSuffix(name, retiredSuffix) +
			": " + err.Error())
	}
	return true
}
