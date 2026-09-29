package cmd

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/user"
)

// TestParseRetentionAcceptsDaysAndDurations pins how a retention window is read off the command
// line. The d suffix is a SwitchTender invention on top of Go duration syntax, so the two forms have
// to be distinguishable and a value that is neither has to be refused rather than silently read as
// zero. Zero is how the flag says "keep everything forever", so a parse that fell back to zero on a
// typo would turn a configured window into unbounded retention with nothing said about it.
func TestParseRetentionAcceptsDaysAndDurations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		In       string
		WantSpan time.Duration
		WantErr  bool
	}{{ // Test 0: Empty means no window at all, which is the documented default.
		Name: "empty", In: "", WantSpan: 0,
	}, { // Test 1: The documented day form counts whole days.
		Name: "ninety days", In: "90d", WantSpan: 90 * 24 * time.Hour,
	}, { // Test 2: One day is the smallest useful day window.
		Name: "one day", In: "1d", WantSpan: 24 * time.Hour,
	}, { // Test 3: Zero days is an explicit zero, not an error.
		Name: "zero days", In: "0d", WantSpan: 0,
	}, { // Test 4: Go duration syntax passes straight through.
		Name: "go duration", In: "72h", WantSpan: 72 * time.Hour,
	}, { // Test 5: A sub-day Go duration is honored as written.
		Name: "minutes", In: "90m", WantSpan: 90 * time.Minute,
	}, { // Test 6: A fractional Go duration is honored as written.
		Name: "fractional hours", In: "1.5h", WantSpan: 90 * time.Minute,
	}, { // Test 7: A bare d has no count in front of it and is refused.
		Name: "bare d", In: "d", WantErr: true,
	}, { // Test 8: A fractional day is not the day form and is refused rather than truncated.
		Name: "fractional days", In: "1.5d", WantErr: true,
	}, { // Test 9: A unitless number is ambiguous and is refused.
		Name: "unitless", In: "90", WantErr: true,
	}, { // Test 10: Nonsense is refused rather than read as no window.
		Name: "garbage", In: "later", WantErr: true,
	}, { // Test 11: A day count that overflows an int is refused.
		Name: "overflowing days", In: "99999999999999999999d", WantErr: true,
	}, { // Test 12: Whitespace is not trimmed, so a padded value is refused rather than guessed at.
		Name: "padded", In: " 90d", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := parseRetention(test.In)
			if (err != nil) != test.WantErr {
				t.Fatalf("%s: parseRetention(%q) error = %v, want error %v",
					test.Name, test.In, err, test.WantErr)
			}
			if test.WantErr {
				return
			}
			if diff := cmp.Diff(test.WantSpan, got); diff != "" {
				t.Errorf("%s: parseRetention(%q) mismatch (-want +got):\n%s", test.Name, test.In, diff)
			}
		})
	}
}

// TestParseRetentionAcceptsANegativeWindow records what a negative retention value does today.
//
// The sweeper only acts when a window is above zero, so a negative one disables retention entirely
// while the flag reads as configured. Nothing is deleted that should have been kept, so this is not
// a data-loss path, but it is the only duration flag on serve that accepts a negative value in
// silence: --evidence-cadence, --span-cadence, and --forward-interval all refuse theirs. The
// behavior is pinned here so a later change to refuse it is a deliberate one.
func TestParseRetentionAcceptsANegativeWindow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		In       string
		WantSpan time.Duration
	}{{ // Test 0: A negative day count parses and comes back negative.
		Name: "negative days", In: "-30d", WantSpan: -30 * 24 * time.Hour,
	}, { // Test 1: A negative Go duration parses and comes back negative.
		Name: "negative hours", In: "-72h", WantSpan: -72 * time.Hour,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := parseRetention(test.In)
			if err != nil {
				t.Fatalf("%s: parseRetention(%q) error = %v", test.Name, test.In, err)
			}
			if diff := cmp.Diff(test.WantSpan, got); diff != "" {
				t.Errorf("%s: parseRetention(%q) mismatch (-want +got):\n%s", test.Name, test.In, diff)
			}
			if got >= 0 {
				t.Errorf("%s: parseRetention(%q) = %s, want a negative window", test.Name, test.In, got)
			}
		})
	}
}

// TestParseRoleMapBindsOnlyRealRoles pins the directory-group-to-role mapping, which decides what a
// person signing in through LDAP, SAML, or a bearer JWT is allowed to do. A group DN contains equals
// signs of its own, so splitting on the first one would map the wrong group; an unrecognized role
// has to be dropped rather than stored, because storing it would hand a matched group a role the
// authorization code has never heard of. Every rejection here fails closed: the entry is absent, so
// the caller falls back to the configured default role instead of gaining one.
func TestParseRoleMapBindsOnlyRealRoles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		In      []string
		WantMap map[string]user.Role
	}{{ // Test 0: No entries is an empty map, not a nil dereference.
		Name: "none", In: nil, WantMap: map[string]user.Role{},
	}, { // Test 1: A plain group maps to its role, lowercased for matching.
		Name: "plain", In: []string{"Platform-Admins=admin"},
		WantMap: map[string]user.Role{"platform-admins": user.RoleAdmin},
	}, { // Test 2: A group DN carries equals signs, so the split is on the last one.
		Name: "group dn", In: []string{"cn=admins,dc=example,dc=com=admin"},
		WantMap: map[string]user.Role{"cn=admins,dc=example,dc=com": user.RoleAdmin},
	}, { // Test 3: Surrounding whitespace is trimmed from both halves.
		Name: "padded", In: []string{"  ops  =  operator  "},
		WantMap: map[string]user.Role{"ops": user.RoleOperator},
	}, { // Test 4: An entry with no equals sign names no role and is dropped.
		Name: "no separator", In: []string{"just-a-group"}, WantMap: map[string]user.Role{},
	}, { // Test 5: A role the authorization code does not know is dropped, not stored.
		Name: "unknown role", In: []string{"ops=superuser"}, WantMap: map[string]user.Role{},
	}, { // Test 6: Roles are case sensitive, so Admin is not admin and is dropped.
		Name: "wrong case role", In: []string{"ops=Admin"}, WantMap: map[string]user.Role{},
	}, { // Test 7: An empty group would match nothing sensible and is dropped.
		Name: "empty group", In: []string{"=admin"}, WantMap: map[string]user.Role{},
	}, { // Test 8: An empty role is not a role and is dropped.
		Name: "empty role", In: []string{"ops="}, WantMap: map[string]user.Role{},
	}, { // Test 9: A later entry for the same group wins, so the last flag given decides.
		Name: "duplicate group", In: []string{"ops=viewer", "ops=operator"},
		WantMap: map[string]user.Role{"ops": user.RoleOperator},
	}, { // Test 10: Valid and invalid entries mix without the bad ones poisoning the good.
		Name: "mixed", In: []string{"ops=operator", "junk", "readers=viewer", "x=root"},
		WantMap: map[string]user.Role{"ops": user.RoleOperator, "readers": user.RoleViewer},
	}, { // Test 11: A unicode group name maps by its lowercase form.
		Name: "unicode group", In: []string{"BÜRO=viewer"},
		WantMap: map[string]user.Role{"büro": user.RoleViewer},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := parseRoleMap(test.In)
			if diff := cmp.Diff(test.WantMap, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s: parseRoleMap(%v) mismatch (-want +got):\n%s", test.Name, test.In, diff)
			}
			for _, role := range got {
				if !user.ValidRole(role) {
					t.Errorf("%s: parseRoleMap stored %q, which is not a role the server enforces",
						test.Name, role)
				}
			}
		})
	}
}

// TestIsLoopbackAddrDecidesTheBootstrapBind pins the address test the serve startup guard turns on.
// A loopback bind with no tokens is allowed to run unauthenticated because only the host can reach
// it; anything else mints a credential first. Reading a public address as loopback would serve an
// open API to the network, so every case that is not provably loopback has to come back false.
func TestIsLoopbackAddrDecidesTheBootstrapBind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Addr     string
		WantLoop bool
	}{{ // Test 0: The default serve bind is loopback.
		Addr: "127.0.0.1:8080", WantLoop: true,
	}, { // Test 1: The whole 127/8 block is loopback.
		Addr: "127.2.3.4:8080", WantLoop: true,
	}, { // Test 2: The IPv6 loopback is loopback.
		Addr: "[::1]:8080", WantLoop: true,
	}, { // Test 3: localhost is loopback.
		Addr: "localhost:8080", WantLoop: true,
	}, { // Test 4: A bare loopback address with no port still reads as loopback.
		Addr: "127.0.0.1", WantLoop: true,
	}, { // Test 5: The IPv4 wildcard binds every interface.
		Addr: "0.0.0.0:8080", WantLoop: false,
	}, { // Test 6: The IPv6 wildcard binds every interface.
		Addr: "[::]:8080", WantLoop: false,
	}, { // Test 7: A port with no host binds every interface.
		Addr: ":8080", WantLoop: false,
	}, { // Test 8: An empty address is not proof of loopback.
		Addr: "", WantLoop: false,
	}, { // Test 9: A routable address is not loopback.
		Addr: "10.0.0.5:8080", WantLoop: false,
	}, { // Test 10: A public address is not loopback.
		Addr: "203.0.113.7:8080", WantLoop: false,
	}, { // Test 11: A hostname that is not localhost is not trusted to resolve locally.
		Addr: "switchtender.example.com:8080", WantLoop: false,
	}, { // Test 12: A name that merely starts with localhost is a different host.
		Addr: "localhost.attacker.example:8080", WantLoop: false,
	}, { // Test 13: The check is case sensitive, so an unexpected spelling fails closed.
		Addr: "LOCALHOST:8080", WantLoop: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := isLoopbackAddr(test.Addr); got != test.WantLoop {
				t.Errorf("isLoopbackAddr(%q) = %v, want %v", test.Addr, got, test.WantLoop)
			}
		})
	}
}

// TestParseReceiptRefusesAnythingThatIsNotAChainPosition pins the parser for the seq:link form the
// Audit-Receipt header carries. A receipt is what a relying party holds against the chain, so a
// malformed one has to be refused outright: accepting a zero or negative sequence would send the
// redemption looking for a position no chain ever has, and accepting an empty link would compare
// the stored hash against nothing and call it a match.
func TestParseReceiptRefusesAnythingThatIsNotAChainPosition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		In       string
		WantSeq  int64
		WantLink string
		WantErr  bool
	}{{ // Test 0: The documented form splits into a sequence and a link.
		Name: "documented form", In: "41:9f2c", WantSeq: 41, WantLink: "9f2c",
	}, { // Test 1: Sequence one is the first entry a chain can hold.
		Name: "first entry", In: "1:aa", WantSeq: 1, WantLink: "aa",
	}, { // Test 2: A full length hash link survives intact.
		Name: "full hash", In: "7:" + strings.Repeat("a", 64), WantSeq: 7,
		WantLink: strings.Repeat("a", 64),
	}, { // Test 3: Only the first colon splits, so a link containing one is kept whole.
		Name: "link with colon", In: "9:sha256:abc", WantSeq: 9, WantLink: "sha256:abc",
	}, { // Test 4: No colon at all is not a receipt.
		Name: "no separator", In: "41", WantErr: true,
	}, { // Test 5: An empty string is not a receipt.
		Name: "empty", In: "", WantErr: true,
	}, { // Test 6: Sequence zero is not a chain position.
		Name: "zero sequence", In: "0:aa", WantErr: true,
	}, { // Test 7: A negative sequence is not a chain position.
		Name: "negative sequence", In: "-1:aa", WantErr: true,
	}, { // Test 8: A non-numeric sequence is refused rather than read as zero.
		Name: "non numeric sequence", In: "x:aa", WantErr: true,
	}, { // Test 9: A missing link cannot be compared against anything.
		Name: "missing link", In: "41:", WantErr: true,
	}, { // Test 10: A missing sequence is refused.
		Name: "missing sequence", In: ":aa", WantErr: true,
	}, { // Test 11: A sequence that overflows int64 is refused rather than wrapping.
		Name: "overflow", In: "99999999999999999999999:aa", WantErr: true,
	}, { // Test 12: A padded sequence is refused rather than silently trimmed.
		Name: "padded sequence", In: " 41:aa", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			seq, link, err := parseReceipt(test.In)
			if (err != nil) != test.WantErr {
				t.Fatalf("%s: parseReceipt(%q) error = %v, want error %v",
					test.Name, test.In, err, test.WantErr)
			}
			if test.WantErr {
				return
			}
			if diff := cmp.Diff(test.WantSeq, seq); diff != "" {
				t.Errorf("%s: sequence mismatch (-want +got):\n%s", test.Name, diff)
			}
			if diff := cmp.Diff(test.WantLink, link); diff != "" {
				t.Errorf("%s: link mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestParseReportTimeAcceptsDatesAndTimestamps pins the change register period parser. The window it
// produces decides which changes an auditor sees, so a value the parser cannot read has to be an
// error rather than a zero time, which would silently widen the period back to the start of the
// Common Era.
func TestParseReportTimeAcceptsDatesAndTimestamps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		In      string
		WantUTC string
		WantErr bool
	}{{ // Test 0: A plain date is read as midnight UTC.
		Name: "date", In: "2026-01-31", WantUTC: "2026-01-31T00:00:00Z",
	}, { // Test 1: An RFC 3339 timestamp is read exactly.
		Name: "rfc3339", In: "2026-01-31T13:45:00Z", WantUTC: "2026-01-31T13:45:00Z",
	}, { // Test 2: An offset timestamp is normalized to UTC.
		Name: "offset", In: "2026-01-31T13:45:00-06:00", WantUTC: "2026-01-31T19:45:00Z",
	}, { // Test 3: A leap day that exists parses.
		Name: "leap day", In: "2024-02-29", WantUTC: "2024-02-29T00:00:00Z",
	}, { // Test 4: A leap day that does not exist is refused rather than rolled forward.
		Name: "impossible leap day", In: "2026-02-29", WantErr: true,
	}, { // Test 5: An empty value is not a time.
		Name: "empty", In: "", WantErr: true,
	}, { // Test 6: A US-style date is refused rather than guessed at.
		Name: "us order", In: "01/31/2026", WantErr: true,
	}, { // Test 7: A date with no zone and a time is not RFC 3339 and is refused.
		Name: "naive timestamp", In: "2026-01-31 13:45:00", WantErr: true,
	}, { // Test 8: A word is not a time.
		Name: "word", In: "yesterday", WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := parseReportTime(test.In)
			if (err != nil) != test.WantErr {
				t.Fatalf("%s: parseReportTime(%q) error = %v, want error %v",
					test.Name, test.In, err, test.WantErr)
			}
			if test.WantErr {
				return
			}
			if diff := cmp.Diff(test.WantUTC, got.UTC().Format(time.RFC3339)); diff != "" {
				t.Errorf("%s: parseReportTime(%q) mismatch (-want +got):\n%s", test.Name, test.In, diff)
			}
		})
	}
}

// TestRedactDSNKeepsAPasswordOutOfTheLog pins the redaction the worker applies before it logs where
// it leases from. The DSN carries the database password, and the worker banner is written on every
// start, so a miss here puts a live credential in the log file and in whatever collects it.
func TestRedactDSNKeepsAPasswordOutOfTheLog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		In       string
		WantOut  string
		WantHide string
	}{{ // Test 0: A postgres DSN loses its user and password.
		Name: "postgres", In: "postgres://alice:s3cret@db.example:5432/st",
		WantOut: "postgres://***@db.example:5432/st", WantHide: "s3cret",
	}, { // Test 1: The postgresql scheme is redacted the same way.
		Name: "postgresql", In: "postgresql://alice:s3cret@db/st",
		WantOut: "postgresql://***@db/st", WantHide: "s3cret",
	}, { // Test 2: A password containing an at sign is still removed, since the split is on the last one.
		Name: "at in password", In: "postgres://alice:p@ss@db/st",
		WantOut: "postgres://***@db/st", WantHide: "p@ss",
	}, { // Test 3: A SQLite path has no credential and passes through unchanged.
		Name: "sqlite path", In: "/var/lib/switchtender/st.db",
		WantOut: "/var/lib/switchtender/st.db",
	}, { // Test 4: A DSN with no credential section is left alone.
		Name: "no credential", In: "postgres://db.example/st", WantOut: "postgres://db.example/st",
	}, { // Test 5: An empty DSN stays empty rather than becoming a redaction marker.
		Name: "empty", In: "", WantOut: "",
	}, { // Test 6: A user with no password is still hidden, since the user is an identity too.
		Name: "user only", In: "postgres://alice@db/st", WantOut: "postgres://***@db/st",
		WantHide: "alice",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := redactDSN(test.In)
			if diff := cmp.Diff(test.WantOut, got); diff != "" {
				t.Errorf("%s: redactDSN(%q) mismatch (-want +got):\n%s", test.Name, test.In, diff)
			}
			if test.WantHide != "" && strings.Contains(got, test.WantHide) {
				t.Errorf("%s: redactDSN(%q) = %q, which still carries %q",
					test.Name, test.In, got, test.WantHide)
			}
		})
	}
}

// TestTrimSpaceBytesHandlesEveryShapeOfPadding pins the trimmer the license mint uses on the issuer
// key file. A key file written by a shell almost always ends in a newline, and a seed that keeps it
// fails hex decoding, so the trim has to cover every ASCII space form and has to survive a file that
// is nothing but padding.
func TestTrimSpaceBytesHandlesEveryShapeOfPadding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		In      string
		WantOut string
	}{{ // Test 0: A trailing newline, the ordinary shape of a key file, is removed.
		Name: "trailing newline", In: "abcd\n", WantOut: "abcd",
	}, { // Test 1: Windows line endings are removed.
		Name: "crlf", In: "abcd\r\n", WantOut: "abcd",
	}, { // Test 2: Padding on both sides is removed.
		Name: "both sides", In: " \t abcd \n\r\t ", WantOut: "abcd",
	}, { // Test 3: A value with no padding is returned untouched.
		Name: "clean", In: "abcd", WantOut: "abcd",
	}, { // Test 4: An empty input stays empty rather than panicking on the bounds.
		Name: "empty", In: "", WantOut: "",
	}, { // Test 5: Nothing but padding trims to empty rather than crossing the indexes.
		Name: "all padding", In: " \t\r\n ", WantOut: "",
	}, { // Test 6: Interior spaces are not touched, so a corrupt key stays visibly corrupt.
		Name: "interior space", In: " ab cd ", WantOut: "ab cd",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := string(trimSpaceBytes([]byte(test.In)))
			if diff := cmp.Diff(test.WantOut, got); diff != "" {
				t.Errorf("%s: trimSpaceBytes(%q) mismatch (-want +got):\n%s", test.Name, test.In, diff)
			}
		})
	}
}

// TestShortDigestNeverGrowsWhatItShortens pins the digest abbreviation the receipt printer uses.
// A reader compares two of these by eye across two documents, so the length has to be stable and a
// digest shorter than the cut has to come back whole rather than out of bounds.
func TestShortDigestNeverGrowsWhatItShortens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		In      string
		WantOut string
	}{{ // Test 0: A full hash is cut to twelve characters.
		Name: "full hash", In: strings.Repeat("a", 64), WantOut: strings.Repeat("a", 12),
	}, { // Test 1: Exactly twelve characters is not cut.
		Name: "exactly twelve", In: "abcdefabcdef", WantOut: "abcdefabcdef",
	}, { // Test 2: Thirteen characters is one past the boundary and is cut.
		Name: "thirteen", In: "abcdefabcdefg", WantOut: "abcdefabcdef",
	}, { // Test 3: A short digest comes back whole.
		Name: "short", In: "abc", WantOut: "abc",
	}, { // Test 4: An empty digest stays empty rather than slicing out of range.
		Name: "empty", In: "", WantOut: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := shortDigest(test.In)
			if diff := cmp.Diff(test.WantOut, got); diff != "" {
				t.Errorf("%s: shortDigest(%q) mismatch (-want +got):\n%s", test.Name, test.In, diff)
			}
			if len(got) > 12 {
				t.Errorf("%s: shortDigest(%q) = %q, longer than the twelve it promises",
					test.Name, test.In, got)
			}
		})
	}
}

// TestMatchWordAgreesWithTheVerdictItCaptions pins the caption verb beside a digest comparison. The
// caption sits on the same line as the OK or FAILED mark, so a verb that disagreed with the mark
// would make the verifier contradict itself on the one line a reader looks at hardest.
func TestMatchWordAgreesWithTheVerdictItCaptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		OK       bool
		WantWord string
	}{{ // Test 0: A passing comparison reads as a match.
		Name: "passing", OK: true, WantWord: "matches",
	}, { // Test 1: A failing comparison says so rather than hedging.
		Name: "failing", OK: false, WantWord: "does not match",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantWord, matchWord(test.OK)); diff != "" {
				t.Errorf("%s: matchWord(%v) mismatch (-want +got):\n%s", test.Name, test.OK, diff)
			}
		})
	}
}

// TestBranchOrDefaultNamesTheUnsetBranch pins the import plan's branch label. The plan is what an
// operator reviews before applying it, and an empty cell there reads as a missing value rather than
// as the deliberate "whatever the remote defaults to" that an unset branch means.
func TestBranchOrDefaultNamesTheUnsetBranch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		In       string
		WantName string
	}{{ // Test 0: An unset branch is named rather than left blank.
		Name: "unset", In: "", WantName: "default branch",
	}, { // Test 1: A named branch is shown as written.
		Name: "named", In: "main", WantName: "main",
	}, { // Test 2: A branch whose name contains a slash survives intact.
		Name: "slashes", In: "release/1.2", WantName: "release/1.2",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantName, branchOrDefault(test.In)); diff != "" {
				t.Errorf("%s: branchOrDefault(%q) mismatch (-want +got):\n%s", test.Name, test.In, diff)
			}
		})
	}
}

// TestProducerInstallIDHandlesAnAbsentIdentity pins the install id an anchor records. serve runs
// without an identity when the key cannot be created, and every anchor call site passes the possibly
// nil identity straight through, so a nil dereference here would take down the beat emitter on
// exactly the installs that already lost their key.
func TestProducerInstallIDHandlesAnAbsentIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Identity   *audit.Identity
		WantAnswer string
	}{{ // Test 0: No identity yields no install id rather than a panic.
		Name: "absent", Identity: nil, WantAnswer: "",
	}, { // Test 1: An identity yields its install id.
		Name: "present", Identity: &audit.Identity{InstallID: "inst-1"}, WantAnswer: "inst-1",
	}, { // Test 2: An identity with an empty install id yields empty, not a placeholder.
		Name: "empty install", Identity: &audit.Identity{}, WantAnswer: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantAnswer, producerInstallID(test.Identity)); diff != "" {
				t.Errorf("%s: producerInstallID mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestRandomHexIsTheRightLengthAndNotRepeated pins the generator behind the encryption key, the
// salt, and the generated admin password. The hex length has to be twice the byte count the caller
// asked for, since the caller is choosing key strength by that number, and two calls must not agree,
// since an install whose key repeats is an install with no key at all.
func TestRandomHexIsTheRightLengthAndNotRepeated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Bytes   int
		WantLen int
	}{{ // Test 0: Zero bytes is an empty string rather than an error.
		Name: "zero", Bytes: 0, WantLen: 0,
	}, { // Test 1: One byte is two hex characters.
		Name: "one", Bytes: 1, WantLen: 2,
	}, { // Test 2: The salt width init uses.
		Name: "salt", Bytes: 16, WantLen: 32,
	}, { // Test 3: The key width init uses.
		Name: "key", Bytes: 32, WantLen: 64,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := randomHex(test.Bytes)
			if err != nil {
				t.Fatalf("%s: randomHex(%d) error = %v", test.Name, test.Bytes, err)
			}
			if len(got) != test.WantLen {
				t.Errorf("%s: randomHex(%d) length = %d, want %d",
					test.Name, test.Bytes, len(got), test.WantLen)
			}
			// Only the real key and salt widths are checked for repetition. A one-byte draw
			// collides once in 256 by design, so asserting on it would be a flaky test rather
			// than a statement about the generator.
			if test.Bytes < 16 {
				return
			}
			again, err := randomHex(test.Bytes)
			if err != nil {
				t.Fatalf("%s: second randomHex(%d) error = %v", test.Name, test.Bytes, err)
			}
			if got == again {
				t.Errorf("%s: randomHex(%d) repeated %q, so the value is not unpredictable",
					test.Name, test.Bytes, got)
			}
		})
	}
}

// TestSystemdUnitStartsTheServerItWasAskedFor pins the generated unit. It is the file an operator
// copies into /etc/systemd/system without reading closely, so the database, the address, and the
// environment file it names have to be the ones init was given, and the hardening line has to
// survive: a unit that quietly dropped NoNewPrivileges would run the whole fleet's automation with
// privilege escalation available to it.
func TestSystemdUnitStartsTheServerItWasAskedFor(t *testing.T) {
	t.Parallel()
	unit := systemdUnit("/var/lib/switchtender/st.db", "127.0.0.1:8080", "/etc/switchtender.env",
		"/opt/switchtender/bin/switchtender", "/var/lib/switchtender")
	wants := []string{
		"EnvironmentFile=/etc/switchtender.env",
		// The binary is the one that generated the unit, not a guess at where it was installed.
		// The hardcoded /usr/local/bin here failed 203/EXEC for every install the published script
		// put in ~/.local/bin, which is what it does whenever /usr/local/bin is not writable.
		"ExecStart=/opt/switchtender/bin/switchtender serve --db /var/lib/switchtender/st.db " +
			"--addr 127.0.0.1:8080",
		// systemd runs a service with a working directory of /, so without this a relative --db
		// resolved to /switchtender.db and the server came up on an empty chain nobody chose.
		"WorkingDirectory=/var/lib/switchtender",
		"NoNewPrivileges=true",
		"Restart=on-failure",
		"WantedBy=multi-user.target",
	}
	for testNum, want := range wants {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(unit, want) {
				t.Errorf("the generated unit is missing %q:\n%s", want, unit)
			}
		})
	}
}
