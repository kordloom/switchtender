package dispatch

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/run"
)

// warnUnreferencedFiles records a warning on r, and the same in the server log as structured
// fields, for each file c's custom type wrote that no env or extra-var injector of the type
// references.
//
// Only a type imported from AWX can have one, since a type defined here is refused for it. The file
// is written all the same, and nothing tells the tool where it is, so a run that fails for want of
// the credential would otherwise give no hint why. The warning names the credential, its type, and
// the file, never what the file holds.
//
// It runs after the type's injection succeeded, so reading the type again from the same source
// finds it: this executor's store, or the definition the control node delivered to a relay worker
// beside the credential. A read that fails anyway costs the warning and nothing else, since the run
// is already set up correctly.
func (d *Dispatcher) warnUnreferencedFiles(ctx context.Context, src secretSource, r *run.Run,
	c *credential.Credential) {
	typ, err := src.credentialType(ctx, c)
	if err != nil || typ == nil {
		return
	}
	for _, file := range typ.UnreferencedFiles() {
		addWarning(r, fmt.Sprintf("credential %q of type %q wrote file %s, which no env or "+
			"extra-var injector of the type references, so nothing told the tool where it is. Hand "+
			"the path over with %s on the type, or delete the file injector", c.Name, typ.Name,
			file, credential.FileRef(file)))
		if d.log != nil {
			d.log.Warn("dispatch: credential type wrote a file no injector references",
				zap.String("run_id", r.ID), zap.String("credential_id", c.ID),
				zap.String("credential_type", typ.Name), zap.String("credential_type_id", typ.ID),
				zap.String("type_origin", typ.Origin), zap.String("file", file))
		}
	}
}
