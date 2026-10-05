package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/util"
)

// maxTriggerError bounds the bytes of a refusal reason a trigger keeps, the bound a schedule keeps
// for its own.
const maxTriggerError = 1000

// noteTriggerError records on the trigger why a delivery started no run. The delivery has already
// been answered, so a record that cannot be written is logged and nothing else: the trigger reads
// stale until the next delivery, which is a smaller harm than failing a response already decided.
func noteTriggerError(ctx context.Context, triggers trigger.Store, id, reason string, log *zap.Logger) {
	reason = util.Clip(util.SafeText(reason), maxTriggerError)
	if err := triggers.RecordRefusal(ctx, id, time.Now(), reason); err != nil {
		log.Error("server: record trigger refusal: " + err.Error())
	}
}

// unansweredPath is the chain path that records a launch refused for a survey nobody was present to
// answer: the source's own path, then the questions, comma separated. The questions come last and
// run to the end of the path, so a variable whose name holds a slash cannot pose as a segment.
func unansweredPath(base string, cause error) string {
	return base + "/refused/survey/" + strings.Join(template.UnansweredVars(cause), ",")
}

// recordUnansweredHook records on the chain that a push trigger's delivery was refused because the
// template's survey has a required question with no usable default, and returns the reason the
// sender and the trigger are told. The entry stands in place of the fire entry, so the trail shows
// the webhook arrived and refused rather than nothing. The sender gets the entry's receipt back the
// way a fire's sender does.
func recordUnansweredHook(w http.ResponseWriter, r *http.Request, audits audit.Store, tg *trigger.Trigger,
	t *template.Template, cause error) (string, error) {
	reason := t.RefuseUnattended("webhook fire", cause).Error()
	if audits == nil {
		return reason, nil
	}
	entry := &audit.Entry{
		ID: audit.NewID(), Actor: "webhook:" + tg.ID, Method: http.MethodPost,
		Path: unansweredPath("/hooks/"+tg.ID, cause),
	}
	if err := audits.Append(r.Context(), entry); err != nil {
		return reason, err
	}
	w.Header().Set(AuditReceiptHeader, audit.Receipt(entry))
	return reason, nil
}
