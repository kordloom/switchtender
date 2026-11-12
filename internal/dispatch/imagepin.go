package dispatch

import (
	"context"
	"errors"
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

// WithRefuseUnpinnedImage makes the dispatcher refuse a submission whose image tag cannot be pinned
// to a digest, because the registry did not answer or no resolver is set, rather than binding the
// run to the tag. An approval then always covers the exact image the run executes in.
func WithRefuseUnpinnedImage(refuse bool) Option {
	return func(c *config) { c.refuseUnpinnedImage = refuse }
}

// pinImage resolves the image r will execute in when it is submitted, and pins it onto r: the run's
// own image, which a template launch has already set from the template, then its project's, then this
// server's default. When the registry answers, the tag is pinned to the digest it serves and the run
// executes by that digest. When it does not, or no resolver is set, a dispatcher that refuses
// unpinned images refuses the submission. One that allows them binds the run to the tag, which the
// approval view says, and the outcome records the digest that was pulled.
//
// The image is part of the spec an approval binds, so it is settled here, before any rule weighs the
// run and before an approver sees it. An image left to resolve at execution was one an approval never
// covered: a project's image could be changed after the approval, and the approved run executed in a
// container nobody released. A tool provided by a plugin runs on the host and takes no image.
//
// The lookup runs within ctx, the context of the request submitting the run. When that request
// ends during the lookup, nobody is waiting for the run any more, so the submission stops with
// ctx's error rather than storing a run bound to a tag nobody tried to pin.
//
// The pull credential is checked here for every run that names one, whatever its tool and whether
// or not its image needs a lookup, so a credential that is not a registry login is refused when the
// run is submitted and never when it executes.
func (d *Dispatcher) pinImage(ctx context.Context, r *run.Run) error {
	if !run.IsBuiltinTool(r.Tool) {
		_, err := d.pullCredential(ctx, r.PullCredentialID)
		return err
	}
	if r.Image == "" && r.ProjectID != "" && d.projects != nil {
		if p, err := d.projects.Get(ctx, r.ProjectID); err == nil && p.Image != "" {
			r.Image, r.PullCredentialID = p.Image, p.PullCredentialID
		}
	}
	if r.Image == "" {
		r.Image = d.defaultImage
	}
	pull, err := d.pullCredential(ctx, r.PullCredentialID)
	if err != nil {
		return err
	}
	if r.Image == "" || imageref.Pinned(r.Image) {
		return nil
	}
	if d.imageResolver == nil {
		return d.unpinnedImage(r.Image, errNoDigestLookup)
	}
	creds, release := d.pullLogin(ctx, pull, r.Image)
	defer release()
	digest, err := d.imageResolver.Digest(ctx, r.Image, creds)
	creds.Password = ""
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("the request ended while the run's image tag was being resolved: %w", cerr)
	}
	if err != nil {
		return d.unpinnedImage(r.Image, err)
	}
	pinned, err := imageref.WithDigest(r.Image, digest)
	if err != nil {
		return d.unpinnedImage(r.Image, err)
	}
	r.Image = pinned
	return nil
}

// errNoDigestLookup is the cause given when a tag is refused because this server resolves no tags.
var errNoDigestLookup = errors.New("image digest lookup is off")

// unpinnedImage settles a run whose image tag could not be pinned to a digest. A dispatcher that
// refuses unpinned images fails the submission, since an approval of a tag covers whatever the tag
// names when the run executes. One that allows them leaves the run bound to the tag.
//
// The submitter is told the cause as the resolver words it, which for a registry that did not
// answer is one fixed sentence. Why the dial failed describes this server's own network, so it
// goes to the server log alone.
func (d *Dispatcher) unpinnedImage(image string, cause error) error {
	fields := []zap.Field{zap.String("image", image)}
	if detail := imageref.NoAnswerDetail(cause); detail != "" {
		fields = append(fields, zap.String("detail", detail))
	}
	if d.refuseUnpinnedImage {
		d.log.Info("dispatch: submission refused, image tag not pinned to a digest: "+cause.Error(),
			fields...)
		return fmt.Errorf("%w: %s: %w: pin the image by @sha256 digest, make its registry reachable, "+
			"or start the server with --allow-unpinned-image", ErrImageUnpinned, image, cause)
	}
	d.log.Info("dispatch: image tag not pinned to a digest: "+cause.Error(), fields...)
	return nil
}

// pullCredential returns the record of the credential a run names to pull its image with, unopened,
// after checking that it is a registry credential. Any other kind is refused before its secret is
// opened and before anything is sent to a registry: a pull credential's value goes to the host an
// image reference names, and whoever submits a run chooses that reference. No id, no credential
// store, and a record that cannot be read each return nil, which leaves a digest lookup anonymous.
func (d *Dispatcher) pullCredential(ctx context.Context, id string) (*credential.Credential, error) {
	if id == "" || d.credentials == nil {
		return nil, nil
	}
	c, err := d.credentials.Get(ctx, id)
	if err != nil {
		d.log.Warn("dispatch: read the pull credential to pin an image: "+err.Error(),
			zap.String("credential", id))
		return nil, nil
	}
	if err := credential.CheckPull(c); err != nil {
		return nil, err
	}
	return c, nil
}

// pullHost returns the registry host c's login is bound to when that host serves image, and the
// empty string otherwise: for a login bound to another registry or to none, for a credential that
// is not a registry login, and for an image reference that cannot be read. A login is used only
// where this returns a host.
func pullHost(c *credential.Credential, image string) string {
	host, err := imageref.RegistryHost(image)
	if err != nil {
		return ""
	}
	if bound := c.RegistryHost(); imageref.SameRegistry(bound, host) {
		return bound
	}
	return ""
}

// pullLogin opens the registry login c for asking image's registry which digest its tag names, and
// returns it bound to the one host c names, with a release for anything opening it minted. A login
// bound to another registry is not opened, and the lookup goes ahead without it. A login that
// cannot be opened leaves the request anonymous too: a registry that needs one refuses it, and the
// run stays bound to its tag.
func (d *Dispatcher) pullLogin(ctx context.Context, c *credential.Credential,
	image string) (imageref.Credentials, func()) {
	release := func() {}
	if c == nil || d.sealer == nil || !d.sealer.Enabled() {
		return imageref.Credentials{}, release
	}
	if pullHost(c, image) == "" {
		d.log.Info("dispatch: pull login left out of the digest lookup: it is bound to another registry",
			zap.String("credential", c.ID), zap.String("registry_host", c.RegistryHost()),
			zap.String("image", image))
		return imageref.Credentials{}, release
	}
	opened, plain, lease, err := d.openCredential(ctx, c.ID)
	if lease != nil {
		release = func() { d.revokeLease(lease) }
	}
	if err != nil {
		d.log.Warn("dispatch: open the pull login to pin an image: "+err.Error(),
			zap.String("credential", c.ID))
		return imageref.Credentials{}, release
	}
	// The host comes from the record the secret was opened with, so a credential edited between the
	// check above and the open is still sent only where its own record binds it.
	host := pullHost(opened, image)
	if host == "" {
		return imageref.Credentials{}, release
	}
	user, pass := credential.RegistryLogin(plain)
	return imageref.Credentials{Username: user, Password: pass, Host: host}, release
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
