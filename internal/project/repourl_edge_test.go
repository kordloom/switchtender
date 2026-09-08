package project

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestValidateRepoURLBoundaries drives every documented refusal and every documented acceptance
// through the one function that decides what the executor is allowed to clone.
//
// This is the gate that keeps a stored project from steering the server at its own loopback
// interface or at a cloud metadata endpoint, so each case here is a refusal that must fail closed.
// The acceptances matter just as much: a validator that refuses an ordinary self-hosted git host
// stops the product working, so the private ranges and the scp-like shorthand are pinned too.
func TestValidateRepoURLBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the repository URL handed to the validator.
		In string
		// Want is the error class expected, nil when the URL must be accepted.
		Want error
	}{{ // Test 0: An ordinary https remote.
		In: "https://github.com/org/repo.git", Want: nil,
	}, { // Test 1: An https remote naming an explicit port.
		In: "https://git.example.com:8443/org/repo.git", Want: nil,
	}, { // Test 2: An ssh remote with a login and no secret.
		In: "ssh://git@github.com/org/repo.git", Want: nil,
	}, { // Test 3: The scp-like shorthand, which git treats as ssh.
		In: "git@github.com:org/repo.git", Want: nil,
	}, { // Test 4: The scp-like shorthand with a port.
		In: "git@github.com:2222:org/repo.git", Want: nil,
	}, { // Test 5: An absolute local path.
		In: "/srv/git/repo.git", Want: nil,
	}, { // Test 6: A relative local path.
		In: "./mirrors/repo.git", Want: nil,
	}, { // Test 7: The file scheme.
		In: "file:///srv/git/repo.git", Want: nil,
	}, { // Test 8: A private RFC 1918 address is a self-hosted git host, not an escape.
		In: "https://192.168.10.5/git/repo.git", Want: nil,
	}, { // Test 9: The other private range behaves the same way.
		In: "https://10.0.0.1/git/repo.git", Want: nil,
	}, { // Test 10: Carrier grade NAT space is left alone as well.
		In: "https://100.64.0.1/git/repo.git", Want: nil,
	}, { // Test 11: An IPv6 unique local address is private, not blocked.
		In: "https://[fd00::1]/git/repo.git", Want: nil,
	}, { // Test 12: A public address.
		In: "https://8.8.8.8/org/repo.git", Want: nil,
	}, { // Test 13: The scheme is compared case-insensitively by the parser.
		In: "HTTPS://github.com/org/repo.git", Want: nil,
	}, { // Test 14: A very long path is content, not a refusal.
		In: "https://github.com/org/" + strings.Repeat("a", 4000) + ".git", Want: nil,
	}, { // Test 15: Empty.
		In: "", Want: ErrBadRepoURL,
	}, { // Test 16: Whitespace only, which trims to empty.
		In: "   \t\n ", Want: ErrBadRepoURL,
	}, { // Test 17: Cleartext http is refused for being unauthenticated.
		In: "http://github.com/org/repo.git", Want: ErrBadRepoURL,
	}, { // Test 18: The git protocol is refused for the same reason.
		In: "git://github.com/org/repo.git", Want: ErrBadRepoURL,
	}, { // Test 19: An unrelated scheme is refused.
		In: "ftp://github.com/org/repo.git", Want: ErrBadRepoURL,
	}, { // Test 20: A scheme with no host at all.
		In: "https://", Want: ErrBadRepoURL,
	}, { // Test 21: An empty authority with a path.
		In: "https:///org/repo.git", Want: ErrBadRepoURL,
	}, { // Test 22: The loopback name.
		In: "https://localhost/x", Want: ErrBadRepoURL,
	}, { // Test 23: The loopback name in a different case.
		In: "https://LocalHost/x", Want: ErrBadRepoURL,
	}, { // Test 24: The cloud metadata name.
		In: "https://metadata.google.internal/x", Want: ErrBadRepoURL,
	}, { // Test 25: The cloud metadata name in a different case.
		In: "https://METADATA.GOOGLE.INTERNAL/x", Want: ErrBadRepoURL,
	}, { // Test 26: The loopback address.
		In: "https://127.0.0.1/x", Want: ErrBadRepoURL,
	}, { // Test 27: Loopback on a port.
		In: "https://127.0.0.1:8080/x", Want: ErrBadRepoURL,
	}, { // Test 28: Anywhere in 127.0.0.0/8 is loopback.
		In: "https://127.9.9.9/x", Want: ErrBadRepoURL,
	}, { // Test 29: The two-part loopback shorthand a resolver honors.
		In: "https://127.1/x", Want: ErrBadRepoURL,
	}, { // Test 30: The three-part loopback shorthand.
		In: "https://127.0.1/x", Want: ErrBadRepoURL,
	}, { // Test 31: The bare 32-bit integer spelling of loopback.
		In: "https://2130706433/x", Want: ErrBadRepoURL,
	}, { // Test 32: The unspecified address.
		In: "https://0.0.0.0/x", Want: ErrBadRepoURL,
	}, { // Test 33: The bare integer spelling of the unspecified address.
		In: "https://0/x", Want: ErrBadRepoURL,
	}, { // Test 34: IPv6 loopback.
		In: "https://[::1]/x", Want: ErrBadRepoURL,
	}, { // Test 35: The IPv6 unspecified address.
		In: "https://[::]/x", Want: ErrBadRepoURL,
	}, { // Test 36: IPv4 loopback written as a v4-mapped IPv6 address.
		In: "https://[::ffff:127.0.0.1]/x", Want: ErrBadRepoURL,
	}, { // Test 37: The cloud metadata address.
		In: "https://169.254.169.254/latest/meta-data/", Want: ErrBadRepoURL,
	}, { // Test 38: The rest of link-local goes with it.
		In: "https://169.254.1.1/x", Want: ErrBadRepoURL,
	}, { // Test 39: IPv6 link-local unicast.
		In: "https://[fe80::1]/x", Want: ErrBadRepoURL,
	}, { // Test 40: Link-local multicast.
		In: "https://224.0.0.251/x", Want: ErrBadRepoURL,
	}, { // Test 41: The scp-like shorthand pointed at metadata.
		In: "git@169.254.169.254:x", Want: ErrBadRepoURL,
	}, { // Test 42: The scp-like shorthand pointed at loopback.
		In: "git@127.0.0.1:x", Want: ErrBadRepoURL,
	}, { // Test 43: A password embedded in an https URL.
		In: "https://oauth2:tok3n@github.com/org/repo.git", Want: ErrBadRepoURL,
	}, { // Test 44: A token used as the https username.
		In: "https://ghp_token@github.com/org/repo.git", Want: ErrBadRepoURL,
	}, { // Test 45: A password on an ssh URL.
		In: "ssh://git:pass@github.com/org/repo.git", Want: ErrBadRepoURL,
	}, { // Test 46: Userinfo on the file scheme is still credentials in a URL.
		In: "file://user:pw@/srv/git/repo.git", Want: ErrBadRepoURL,
	}, { // Test 47: A value that looks like a command option, not a repository.
		In: "--upload-pack=/bin/sh", Want: ErrBadRepoURL,
	}, { // Test 48: A short option.
		In: "-u", Want: ErrBadRepoURL,
	}, { // Test 49: A config option that would reach a proxy command.
		In: "--config=core.gitProxy=/bin/sh", Want: ErrBadRepoURL,
	}, { // Test 50: A URL the parser cannot read at all.
		In: "https://bad\x7furl/repo.git", Want: ErrBadRepoURL,
	}, { // Test 51: A bare scheme separator with nothing before it.
		In: "://github.com/x", Want: ErrBadRepoURL,
	}, { // Test 52: A port that is not a number fails the parse rather than being guessed at.
		In: "https://github.com:port/org/repo.git", Want: ErrBadRepoURL,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := ValidateRepoURL(test.In)
			if !errors.Is(err, test.Want) {
				t.Errorf("ValidateRepoURL(%q) error = %v, want %v", test.In, err, test.Want)
			}
		})
	}
}

// TestRepoURLPartsClassification pins how a raw value is split into a host and a transport, which is
// the step every later refusal depends on. A value classified as a local file path skips the host
// checks entirely, so a misclassification is how an address reaches the executor unexamined.
func TestRepoURLPartsClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the raw repository value.
		In string
		// WantHost is the host the validator extracts, empty for a local path.
		WantHost string
		// WantScheme is the transport the validator infers.
		WantScheme string
		// Want is the error class expected, nil when classification succeeds.
		Want error
	}{{ // Test 0: A scheme-prefixed URL yields its own host and scheme.
		In: "https://github.com/org/repo.git", WantHost: "github.com", WantScheme: "https",
	}, { // Test 1: A port is not part of the host.
		In: "ssh://git@example.com:2222/x", WantHost: "example.com", WantScheme: "ssh",
	}, { // Test 2: The scp-like shorthand is ssh with the login stripped.
		In: "git@github.com:org/repo.git", WantHost: "github.com", WantScheme: "ssh",
	}, { // Test 3: The shorthand without a login still names a host.
		In: "github.com:org/repo.git", WantHost: "github.com", WantScheme: "ssh",
	}, { // Test 4: A slash before the colon makes it a local path, as git reads it.
		In: "/srv/git/re:po.git", WantHost: "", WantScheme: "file",
	}, { // Test 5: A plain path with no colon is local.
		In: "/srv/git/repo.git", WantHost: "", WantScheme: "file",
	}, { // Test 6: A relative path is local.
		In: "mirrors/repo.git", WantHost: "", WantScheme: "file",
	}, { // Test 7: A leading dash is refused before anything is inferred from it.
		In: "--upload-pack=/bin/sh", Want: ErrBadRepoURL,
	}, { // Test 8: An unparseable scheme-prefixed value is an error, not a guess.
		In: "https://bad\x7furl/x", Want: ErrBadRepoURL,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			host, scheme, err := repoURLParts(test.In)
			if !errors.Is(err, test.Want) {
				t.Fatalf("repoURLParts(%q) error = %v, want %v", test.In, err, test.Want)
			}
			if test.Want != nil {
				return
			}
			if diff := cmp.Diff(test.WantHost, host); diff != "" {
				t.Errorf("host mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantScheme, scheme); diff != "" {
				t.Errorf("scheme mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestParseLooseIPShorthands pins the shorthand address spellings the validator has to understand.
//
// net.ParseIP returns nil for "127.1" and for "2130706433", and both reach the loopback interface, so
// a validator that only understands the canonical four-part spelling blocks the obvious way in and
// leaves the others open. Each accepted form here is one the host check then gets to refuse.
func TestParseLooseIPShorthands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the host text.
		In string
		// WantIP is the address it parses to, empty when it is not an address at all.
		WantIP string
	}{
		{In: "127.0.0.1", WantIP: "127.0.0.1"},        // Test 0: The canonical spelling.
		{In: "127.1", WantIP: "127.0.0.1"},            // Test 1: Two-part shorthand.
		{In: "127.0.1", WantIP: "127.0.0.1"},          // Test 2: Three-part shorthand.
		{In: "2130706433", WantIP: "127.0.0.1"},       // Test 3: Bare 32-bit integer.
		{In: "0", WantIP: "0.0.0.0"},                  // Test 4: Zero is the unspecified address.
		{In: "4294967295", WantIP: "255.255.255.255"}, // Test 5: The largest 32-bit value.
		{In: "::1", WantIP: "::1"},                    // Test 6: IPv6 loopback.
		{In: "8.8.8.8", WantIP: "8.8.8.8"},            // Test 7: An ordinary public address.
		{In: "github.com", WantIP: ""},                // Test 8: A name is not an address.
		{In: "", WantIP: ""},                          // Test 9: Empty.
		{In: "127", WantIP: "0.0.0.127"},              // Test 10: A bare number is the integer form.
		{In: "1.2.3.4.5", WantIP: ""},                 // Test 11: Five parts is not an address.
		{In: "300.1", WantIP: ""},                     // Test 12: A leading octet above 255.
		{In: "1.99999999999", WantIP: ""},             // Test 13: A trailing part beyond its width.
		{In: "4294967296", WantIP: ""},                // Test 14: Beyond 32 bits.
		{In: "-1", WantIP: ""},                        // Test 15: Negative.
		{In: "1.2.-3", WantIP: ""},                    // Test 16: A negative part.
		{In: "1..2", WantIP: ""},                      // Test 17: An empty part.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := parseLooseIP(test.In)
			if test.WantIP == "" {
				if got != nil {
					t.Errorf("parseLooseIP(%q) = %v, want nil", test.In, got)
				}
				return
			}
			want := net.ParseIP(test.WantIP)
			if got == nil || !got.Equal(want) {
				t.Errorf("parseLooseIP(%q) = %v, want %v", test.In, got, want)
			}
		})
	}
}

// TestCheckRepoUserinfoRefusals pins that a secret carried in a scheme-prefixed URL is refused. A
// token or password written into the remote surfaces in clone and fetch errors and in stored project
// rows, so it belongs in a stored credential rather than in the URL.
func TestCheckRepoUserinfoRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the repository URL.
		In string
		// Want is the error class expected, nil when the URL carries no secret.
		Want error
	}{
		{In: "ssh://git@github.com/org/repo.git", Want: nil}, // Test 0: An ssh login alone.
		{In: "https://github.com/org/repo.git", Want: nil},   // Test 1: No userinfo.
		{In: "git@github.com:org/repo.git", Want: nil},       // Test 2: No scheme at all.
		{In: "/srv/git/repo.git", Want: nil},                 // Test 3: A local path.
		{In: "ssh://git:pw@h/x", Want: ErrBadRepoURL},        // Test 4: Password on ssh.
		{In: "ssh://git:@h/x", Want: ErrBadRepoURL},          // Test 5: Empty password still set.
		{In: "https://tok@h/x", Want: ErrBadRepoURL},         // Test 6: Token as https user.
		{In: "https://u:p@h/x", Want: ErrBadRepoURL},         // Test 7: Password on https.
		{In: "file://u@/srv/x", Want: ErrBadRepoURL},         // Test 8: Userinfo on file.
		{In: "https://bad\x7furl@h/x", Want: nil},            // Test 9: Unparseable defers.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := checkRepoUserinfo(test.In); !errors.Is(err, test.Want) {
				t.Errorf("checkRepoUserinfo(%q) error = %v, want %v", test.In, err, test.Want)
			}
		})
	}
}

// TestValidateRepoURLRefusesLoopbackBehindScpUserinfo demonstrates a bypass of the address refusal.
//
// go-git splits the scp-like shorthand at the last "@" before the host, so "u:p@127.0.0.1:repo.git"
// names host 127.0.0.1 and is dialed there. repoURLParts splits at the first colon instead, reads
// the host as "u", and hands that to checkRepoHost, which finds nothing wrong with it. The URL names
// an address rather than a name, which is exactly the case the text check is supposed to settle, and
// the ssh transport carries no dial-time guard to catch it afterward.
func TestValidateRepoURLRefusesLoopbackBehindScpUserinfo(t *testing.T) {
	t.Parallel()
	refused := []string{
		"u:p@127.0.0.1:repo.git",
		"u:p@169.254.169.254:repo.git",
		"deploy:token@localhost:repo.git",
		"a:b@2130706433:repo.git",
	}
	for testNum, raw := range refused {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := ValidateRepoURL(raw); !errors.Is(err, ErrBadRepoURL) {
				t.Errorf("ValidateRepoURL(%q) error = %v, want ErrBadRepoURL: go-git dials the host "+
					"after the last @, which the check never looked at", raw, err)
			}
		})
	}
}

// TestRedactRepoURLStripsScpShorthandCredentials demonstrates a credential leak in error text.
//
// redactRepoURL passes the scp-like shorthand through unchanged on the grounds that the shorthand
// has no password slot. go-git's own parse disagrees: everything before the last "@" is the user, so
// "tok:secret@git.example.com:repo.git" is a remote it will dial with that whole string as the login.
// ValidateRepoURL accepts it, and every clone and fetch failure interpolates it, so the secret lands
// in logs, run records, and API responses.
func TestRedactRepoURLStripsScpShorthandCredentials(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3tvalue"
	raw := "tok:" + secret + "@git.example.com:team/infra.git"
	got := redactRepoURL(raw)
	if strings.Contains(got, secret) {
		t.Errorf("redactRepoURL(%q) = %q, which still carries the secret into error text", raw, got)
	}
}

// TestParseLooseIPHonorsOctalAndHexOctets demonstrates two more spellings of loopback that a
// resolver honors and the validator does not.
//
// parseLooseIP exists to accept "the shorthand forms a resolver honors but net.ParseIP does not".
// inet_aton, which the platform resolver uses on macOS and on glibc, also reads a part with a leading
// zero as octal and a "0x" part as hex, so "0177.0.0.1" and "0x7f.0.0.1" both reach 127.0.0.1. Both
// parse to nil here, so checkRepoHost never gets to refuse them.
func TestParseLooseIPHonorsOctalAndHexOctets(t *testing.T) {
	t.Parallel()
	for testNum, host := range []string{"0177.0.0.1", "0x7f.0.0.1", "0177.1", "0x7f000001"} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ip := parseLooseIP(host)
			if ip == nil || !ip.IsLoopback() {
				t.Errorf("parseLooseIP(%q) = %v, want the loopback address a resolver reaches", host, ip)
			}
		})
	}
}
