package witnessclient

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kordloom/switchtender/witness"
)

// Status is a client's account of its enrollment and its pushes since the process started: the body
// of the status route and the line the server logs at start and on every change.
type Status struct {
	// Witness is the base URL of the witness the install is enrolled with.
	Witness string `json:"witness"`
	// WitnessKeyID is the witness key the install's enrollment names as current.
	WitnessKeyID string `json:"witness_key_id"`
	// InstallID is the install's label, the id its producer key was born to.
	InstallID string `json:"install_id"`
	// KeyID is the producer key id the witness keeps this install's memory under.
	KeyID string `json:"key_id"`
	// EnrolledAt is when the witness first enrolled the install.
	EnrolledAt string `json:"enrolled_at"`
	// LastBeat is the newest beat the witness countersigned, zero before the first.
	LastBeat int64 `json:"last_beat"`
	// LastSeq is that beat's position in the chain.
	LastSeq int64 `json:"last_seq"`
	// LastCountersignedAt is when the witness countersigned that beat, by its own signed time.
	LastCountersignedAt string `json:"last_countersigned_at,omitempty"`
	// Queue is how many pushes are waiting for the witness.
	Queue int `json:"queue"`
	// QueueCap is how many pushes the queue holds before it drops the oldest.
	QueueCap int `json:"queue_cap"`
	// Dropped counts pushes dropped from a full queue.
	Dropped int64 `json:"dropped"`
	// Stale counts beats the witness already held a tree at or past, dropped and never sent again.
	Stale int64 `json:"stale"`
	// LastError says why the most recent push that was not countersigned was not, empty before one.
	LastError string `json:"last_error,omitempty"`
	// LastErrorAt is when that push was answered, or found the witness unreachable.
	LastErrorAt string `json:"last_error_at,omitempty"`
	// LastRefusal is the most recent answer in which the witness refused a push, findings verbatim.
	LastRefusal *Testimony `json:"last_refusal,omitempty"`
	// LastNote is the most recent countersigned answer that carried notes about the witness's
	// coverage, such as beats it never received.
	LastNote *Testimony `json:"last_note,omitempty"`
	// Lapsed is whether the witness said the install's license and its grace have ended. The client
	// pushes nothing more once it has.
	Lapsed bool `json:"lapsed"`
}

// Testimony is one witness answer that carried findings: the beat it was about and what the witness
// said, in the shape the witness records it.
type Testimony struct {
	// At is when the answer arrived, by this install's clock.
	At string `json:"at"`
	// Beat is the pushed beat's number.
	Beat int64 `json:"beat"`
	// Seq is the pushed beat's position in the chain.
	Seq int64 `json:"seq"`
	// Findings are what the witness said, each with its class.
	Findings []witness.RecordedFinding `json:"findings"`
}

// clone returns a copy of t that shares no slice with it.
func (t *Testimony) clone() *Testimony {
	if t == nil {
		return nil
	}
	c := *t
	c.Findings = slices.Clone(t.Findings)
	return &c
}

// Line returns the status as one log line.
func (s Status) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "witness %s: install %s, producer key %s, witness key %s", s.Witness,
		s.InstallID, s.KeyID, s.WitnessKeyID)
	if s.LastBeat > 0 {
		fmt.Fprintf(&b, ", last countersigned beat %d at seq %d (%s)", s.LastBeat, s.LastSeq,
			s.LastCountersignedAt)
	} else {
		b.WriteString(", nothing countersigned since start")
	}
	fmt.Fprintf(&b, ", queue %d of %d, dropped %d, stale %d", s.Queue, s.QueueCap, s.Dropped,
		s.Stale)
	if s.Lapsed {
		b.WriteString(", lapsed: pushing stopped")
	}
	if s.LastError != "" {
		fmt.Fprintf(&b, ", last error at %s: %s", s.LastErrorAt, s.LastError)
	}
	return b.String()
}
