// Package imageref reads container image references and resolves a tag to the digest a registry
// serves for it, so a run can be pinned to the exact image it will execute in.
package imageref

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// dockerHubDomain is the domain an image reference with no registry names, and dockerHubRegistry is
// where that registry actually serves its API.
const (
	dockerHubDomain   = "docker.io"
	dockerHubRegistry = "registry-1.docker.io"
)

// dockerHubHosts lists every host name that stands for Docker Hub's registry, so a login bound to
// docker.io matches an image the hub serves under any of them. The list is fixed: no other registry
// has an alias here.
var dockerHubHosts = []string{dockerHubDomain, "index.docker.io", dockerHubRegistry}

// ErrReference is returned for an image reference this package cannot read.
var ErrReference = errors.New("invalid image reference")

// digestPattern is a sha256 manifest digest, the only kind a pinned reference may carry.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Ref is a parsed image reference.
type Ref struct {
	// Domain is the registry the reference names, docker.io when it names none.
	Domain string
	// Path is the repository within the registry, with library/ added for a Docker Hub official image.
	Path string
	// Tag is the tag the reference names, latest when it names neither a tag nor a digest.
	Tag string
	// Digest is the digest the reference pins, empty when it names a tag alone.
	Digest string
	// Name is the reference as written, less its tag and digest.
	Name string
}

// Parse reads an image reference the way the container runtime does: an optional registry domain,
// recognized by a dot, a port, or the name localhost, then the repository path, then an optional tag
// and an optional digest.
func Parse(ref string) (Ref, error) {
	if ref == "" || strings.ContainsAny(ref, " \t\n") {
		return Ref{}, fmt.Errorf("%w: %q", ErrReference, ref)
	}
	var out Ref
	rest := ref
	if name, digest, ok := strings.Cut(ref, "@"); ok {
		if !digestPattern.MatchString(digest) {
			return Ref{}, fmt.Errorf("%w: %q carries a digest that is not sha256", ErrReference, ref)
		}
		out.Digest, rest = digest, name
	}
	if slash := strings.LastIndex(rest, "/"); strings.LastIndex(rest, ":") > slash {
		at := strings.LastIndex(rest, ":")
		out.Tag, rest = rest[at+1:], rest[:at]
	}
	out.Name = rest
	first, remainder, hasSlash := strings.Cut(rest, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		out.Domain, out.Path = first, remainder
	} else {
		out.Domain, out.Path = dockerHubDomain, rest
		if !strings.Contains(rest, "/") {
			out.Path = "library/" + rest
		}
	}
	if out.Path == "" || strings.HasSuffix(rest, ":") || strings.HasSuffix(ref, ":") {
		return Ref{}, fmt.Errorf("%w: %q", ErrReference, ref)
	}
	if out.Tag == "" && out.Digest == "" {
		out.Tag = "latest"
	}
	return out, nil
}

// Pinned reports whether ref names its image by digest.
func Pinned(ref string) bool {
	r, err := Parse(ref)
	return err == nil && r.Digest != ""
}

// DigestOf returns the digest ref pins, or the empty string when it pins none.
func DigestOf(ref string) string {
	r, err := Parse(ref)
	if err != nil {
		return ""
	}
	return r.Digest
}

// WithDigest returns ref pinned to digest, keeping its tag so a reader still sees which tag it was.
func WithDigest(ref, digest string) (string, error) {
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("%w: digest %q is not sha256", ErrReference, digest)
	}
	r, err := Parse(ref)
	if err != nil {
		return "", err
	}
	if r.Digest != "" {
		return ref, nil
	}
	return strings.TrimSuffix(ref, "@") + "@" + digest, nil
}

// registryHost returns the host serving the registry API for r.
func (r Ref) registryHost() string {
	if r.Domain == dockerHubDomain {
		return dockerHubRegistry
	}
	return r.Domain
}

// RegistryHost returns the host that serves the registry API for ref, with its port when ref names
// one. It is the host a pull login for the image would be sent to.
func RegistryHost(ref string) (string, error) {
	r, err := Parse(ref)
	if err != nil {
		return "", err
	}
	return r.registryHost(), nil
}

// SameRegistry reports whether a and b name one registry host. Hosts compare without regard to
// case and a port is part of the host, so registry.example.com and registry.example.com:5000 are
// two registries. Docker Hub's host names all stand for one registry. An empty host matches
// nothing.
func SameRegistry(a, b string) bool {
	a, b = canonicalHost(a), canonicalHost(b)
	return a != "" && a == b
}

// canonicalHost lowers host and folds Docker Hub's host names into docker.io.
func canonicalHost(host string) string {
	host = strings.ToLower(host)
	if slices.Contains(dockerHubHosts, host) {
		return dockerHubDomain
	}
	return host
}
