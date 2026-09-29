// Package project sources playbooks from git repositories. A project names a repository and
// branch; runs reference the project plus a path inside it, and every run records the exact
// commit it executed, so history answers what version ran, not just what file name.
package project

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

var (
	// ErrNotFound is returned when a project does not exist in the store.
	ErrNotFound = errors.New("project not found")
	// ErrEscapesRepo is returned when a run path points outside its project's repository.
	ErrEscapesRepo = errors.New("path escapes the repository")
	// ErrBadRepoURL is returned when a repository URL uses a disallowed scheme or points at a
	// blocked host, such as loopback or a cloud metadata address.
	ErrBadRepoURL = errors.New("invalid repository url")
)

// Project is one git-sourced playbook repository.
type Project struct {
	// ID is the unique project identifier.
	ID string `json:"id"`
	// Name labels the project for humans.
	Name string `json:"name"`
	// RepoURL is the git remote: ssh, https, or a local path.
	RepoURL string `json:"repo_url"`
	// Branch is the branch to sync. Empty means the remote default.
	Branch string `json:"branch,omitempty"`
	// CredentialID names an ssh_key credential for private remotes. Empty for public or local.
	CredentialID string `json:"credential_id,omitempty"`
	// InstallDeps installs the project's Ansible role and collection requirements on each sync so
	// playbooks that need them run without manual setup. It defaults to true.
	InstallDeps bool `json:"install_deps"`
	// Image, when set, names a container image the project's runs execute inside, pinning its own
	// ansible and system dependencies. Empty runs on the host.
	Image string `json:"image,omitempty"`
	// PullCredentialID names a registry credential for pulling a private Image. Empty for public.
	PullCredentialID string `json:"pull_credential_id,omitempty"`
	// OrgID is the owning organization. Empty means unowned, a global object that follows the role.
	// When set, members of the organization gain access to the project and, under strict grants, it
	// is hidden from non-members who lack an explicit grant.
	OrgID string `json:"org_id,omitempty"`
	// CreatedAt is when the project was created.
	CreatedAt time.Time `json:"created_at"`
}

// Store persists projects. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the project identified by p.ID.
	Save(ctx context.Context, p *Project) error
	// Update changes an existing project's mutable fields, preserving its creation time, or returns
	// ErrNotFound.
	Update(ctx context.Context, p *Project) error
	// Get returns the project with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Project, error)
	// List returns all projects ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Project, error)
	// Delete removes the project with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// NewID returns a random project identifier prefixed with "proj_".
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("project: read random: " + err.Error())
	}
	return "proj_" + hex.EncodeToString(b[:])
}
