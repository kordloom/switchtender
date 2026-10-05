package schedule

import (
	"os"
	"strings"
	"sync"
	"time"
)

// UnnamedZone is the zone a schedule that names none is read in, and the zone the stores write onto
// a row an older release left without one.
//
// Such a schedule used to be read in the zone of whichever server happened to evaluate it. A highly
// available pair claims each due occurrence with a compare-and-set, but the winner of a claim also
// computes the next fire, so a pair whose servers sat in different zones moved a daily 09:00
// between the two readings and ran it twice on one day. A stored row records nothing about the zone
// its author meant, so the one reading that cannot differ between servers, releases, or upgrade
// order is a fixed one, and UTC is the zone the published container image and Helm chart already
// run in.
const UnnamedZone = "UTC"

var (
	// serverZoneOnce resolves the server's zone once per process.
	serverZoneOnce sync.Once
	// serverZone is the resolved name ServerZone returns.
	serverZone string
)

// ServerZone returns the IANA name of the zone this server's clock is set to, such as
// America/Chicago, which is the zone a schedule created or imported without one is pinned to.
// Pinning writes the name onto the schedule, so the server that created it and every other server
// read it the same way afterward.
//
// The name comes from TZ when it is set, then from the zone file /etc/localtime links to, then from
// /etc/timezone when it agrees with the clock. A server with no zone information runs in UTC, as Go
// itself does, and so does one whose zone file was copied rather than linked and carries no name:
// the zone cannot be written onto a schedule without a name, and a fixed answer is better than one
// that changes with the server reading it. Set TZ to name such a server's zone.
func ServerZone() string {
	serverZoneOnce.Do(func() {
		serverZone = resolveServerZone(zoneSources{
			lookupEnv: os.LookupEnv, readlink: os.Readlink, readFile: os.ReadFile, local: time.Local,
		})
	})
	return serverZone
}

// zoneSources are the places a server's zone name is read from, held apart so a test can supply
// them.
type zoneSources struct {
	// lookupEnv reads an environment variable and reports whether it is set.
	lookupEnv func(string) (string, bool)
	// readlink reads the target of a symbolic link.
	readlink func(string) (string, error)
	// readFile reads a whole file.
	readFile func(string) ([]byte, error)
	// local is the zone the process's clock reads in.
	local *time.Location
}

// resolveServerZone names the zone src describes, or UnnamedZone when it names none.
func resolveServerZone(src zoneSources) string {
	if tz, ok := src.lookupEnv("TZ"); ok {
		return zoneFromTZ(tz, src)
	}
	if name := zoneFromLink("/etc/localtime", src); name != "" {
		return name
	}
	if data, err := src.readFile("/etc/timezone"); err == nil {
		name := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
		if loadableZone(name) && sameZone(name, src.local) {
			return name
		}
	}
	return UnnamedZone
}

// zoneFromTZ names the zone a TZ value selects, read the way Go reads it: empty is UTC, a leading
// colon is dropped, an absolute path is a zone file, and anything that does not load leaves the
// clock in UTC.
func zoneFromTZ(tz string, src zoneSources) string {
	tz = strings.TrimPrefix(tz, ":")
	switch {
	case tz == "":
		return UnnamedZone
	case strings.HasPrefix(tz, "/"):
		if name := zoneFromPath(tz); name != "" {
			return name
		}
		if name := zoneFromLink(tz, src); name != "" {
			return name
		}
		return UnnamedZone
	case loadableZone(tz):
		return tz
	}
	return UnnamedZone
}

// zoneFromLink names the zone a symbolic link to a zone file selects, or returns the empty string
// when path is not such a link.
func zoneFromLink(path string, src zoneSources) string {
	target, err := src.readlink(path)
	if err != nil {
		return ""
	}
	return zoneFromPath(target)
}

// zoneFromPath names the zone a path inside a zone database selects, such as
// /usr/share/zoneinfo/America/Chicago, or returns the empty string when the path is not inside one
// or names no zone this system can load.
func zoneFromPath(path string) string {
	_, name, found := strings.Cut(path, "zoneinfo/")
	if !found {
		return ""
	}
	for _, variant := range []string{"posix/", "right/"} {
		name = strings.TrimPrefix(name, variant)
	}
	if !loadableZone(name) {
		return ""
	}
	return name
}

// loadableZone reports whether name is a zone name this system can load.
func loadableZone(name string) bool {
	if name == "" || name == "Local" || strings.ContainsAny(name, " \t\r\n=") {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// sameZone reports whether the named zone and loc give the same offset across the coming year and
// the one before it, which is how a stale /etc/timezone left behind by an earlier setting is told
// apart from the zone the clock actually reads in.
func sameZone(name string, loc *time.Location) bool {
	named, err := time.LoadLocation(name)
	if err != nil || loc == nil {
		return false
	}
	now := time.Now()
	for months := -12; months <= 12; months++ {
		at := now.AddDate(0, months, 0)
		_, a := at.In(named).Zone()
		_, b := at.In(loc).Zone()
		if a != b {
			return false
		}
	}
	return true
}
