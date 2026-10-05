package dispatch

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/imageref"
	"github.com/kordloom/switchtender/internal/run"
)

// ImageResolver resolves an image tag to the digest its registry serves.
type ImageResolver interface {
	// Digest returns the digest the registry serves for ref, logging in with creds when the registry
	// asks for a login.
	Digest(ctx context.Context, ref string, creds imageref.Credentials) (string, error)
}

// WithImageResolver makes the dispatcher pin each submitted run's image to the digest its tag
// resolves to, so the run executes by that digest. Without it every image stays bound to its tag.
func WithImageResolver(r ImageResolver) Option {
	return func(c *config) { c.imageResolver = r }
}

// pinImage resolves the image r will execute in when it is submitted, and pins it onto r: the run's
// own image, which a template launch has already set from the template, then its project's, then this
// server's default. When the registry answers, the tag is pinned to the digest it serves and the run
// executes by that digest. When it does not, the run stays bound to the tag, which the approval view
// says, and the outcome records the digest that was pulled.
//
// The image is part of the spec an approval binds, so it is settled here, before any rule weighs the
// run and before an approver sees it. An image left to resolve at execution was one an approval never
// covered: a project's image could be changed after the approval, and the approved run executed in a
// container nobody released. A tool provided by a plugin runs on the host and takes no image.
//
// The lookup runs within ctx, the context of the request submitting the run. When that request
// ends during the lookup, nobody is waiting for the run any more, so the submission stops with
// ctx's error rather than storing a run bound to a tag nobody tried to pin.
func (d *Dispatcher) pinImage(ctx context.Context, r *run.Run) error {
	if !run.IsBuiltinTool(r.Tool) {
		return nil
	}
	if r.Image == "" && r.ProjectID != "" && d.projects != nil {
		if p, err := d.projects.Get(ctx, r.ProjectID); err == nil && p.Image != "" {
			r.Image, r.PullCredentialID = p.Image, p.PullCredentialID
		}
	}
	if r.Image == "" {
		r.Image = d.defaultImage
	}
	if r.Image == "" || imageref.Pinned(r.Image) || d.imageResolver == nil {
		return nil
	}
	creds, release := d.pullLogin(ctx, r.PullCredentialID)
	defer release()
	digest, err := d.imageResolver.Digest(ctx, r.Image, creds)
	creds.Password = ""
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("the request ended while the run's image tag was being resolved: %w", cerr)
	}
	if err != nil {
		d.log.Info("dispatch: image tag not pinned to a digest: "+err.Error(),
			zap.String("image", r.Image))
		return nil
	}
	pinned, err := imageref.WithDigest(r.Image, digest)
	if err != nil {
		d.log.Info("dispatch: image tag not pinned to a digest: "+err.Error(),
			zap.String("image", r.Image))
		return nil
	}
	r.Image = pinned
	return nil
}

// pullLogin opens the registry login a run pulls its image with, for asking the registry which digest
// a tag names, and returns it with a release for anything opening it minted. A login that cannot be
// opened leaves the request anonymous: a registry that needs one refuses it, and the run stays bound
// to its tag.
func (d *Dispatcher) pullLogin(ctx context.Context, id string) (imageref.Credentials, func()) {
	release := func() {}
	if id == "" || d.credentials == nil || d.sealer == nil || !d.sealer.Enabled() {
		return imageref.Credentials{}, release
	}
	_, plain, lease, err := d.openCredential(ctx, id)
	if lease != nil {
		release = func() { d.revokeLease(lease) }
	}
	if err != nil {
		d.log.Warn("dispatch: open the pull login to pin an image: "+err.Error(),
			zap.String("credential", id))
		return imageref.Credentials{}, release
	}
	user, pass := credential.RegistryLogin(plain)
	return imageref.Credentials{Username: user, Password: pass}, release
}

// checkImagePin refuses an approved run whose image at execution is not the image it was approved
// with. The approval bound the image pinned when the run was submitted. An executor that would run it
// elsewhere, on the host when it was approved to run in a container, or in this executor's own default
// image when it was approved to run on the host, would execute something nobody released.
func checkImagePin(r *run.Run, effective string) error {
	if r.ApprovedSpecBinding == "" || effective == r.Image {
		return nil
	}
	approved := r.Image
	if approved == "" {
		approved = "no image, on the executor's host"
	}
	if effective == "" {
		effective = "no image, on this executor's host"
	}
	return fmt.Errorf("%w: this run was approved to run in %s, and this executor would run it in %s",
		ErrImagePin, approved, effective)
}
