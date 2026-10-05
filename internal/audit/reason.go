package audit

// MethodReason is the method recorded for the entries that follow an approval decision's reason
// after the decision itself: a correction appended to it, and a redaction that removed its text.
// The decision entry commits the reason, and these record what happened to it afterward without
// ever editing the entry that committed it.
const MethodReason = "REASON"
