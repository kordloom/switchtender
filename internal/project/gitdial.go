package project

import (
	"net/http"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/kordloom/switchtender/internal/safedial"
)

// init guards the HTTPS transport go-git dials with, so a repository URL cannot reach an address
// ValidateRepoURL would have refused.
//
// The URL check reads the host as written, which settles the question only for a URL that names an
// address. A name is a different matter: it can resolve to 127.0.0.1 or to a metadata endpoint, and
// it can resolve to one thing when the project is saved and another when the clone runs, so a check
// performed on the text can always be walked around. The refusal has to happen once the address is
// known, which is at the dial, and this is where go-git does its dialing.
//
// The off-host policy is used rather than the plain one because it is the policy the URL check
// already expresses: checkRepoHost refuses loopback, the unspecified address, and link-local, and
// leaves the private ranges alone so a git host inside the network still clones.
//
// Only HTTPS is installed. It is the scheme that reaches the services this protects against, since
// a metadata endpoint and an internal admin API both answer HTTP, and it is the one go-git lets a
// caller hand a client to. An SSH remote still gets the name check, and an SSH daemon is not a
// useful target for a request forgery, so the gap is narrow and stated rather than hidden.
func init() {
	client.InstallProtocol("https", githttp.NewClient(&http.Client{
		Transport: safedial.OffHostTransport(),
	}))
}
