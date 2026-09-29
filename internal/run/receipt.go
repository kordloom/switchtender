package run

import "context"

// receiptKey types the context value carrying the audit receipt of the request in flight.
type receiptKey struct{}

// WithAuditReceipt carries the audit receipt minted for the request being served, so a run created
// while handling it can record which chain entry authorized its creation. The entry is appended
// before the handler runs and names the request path, which cannot name a run that does not exist
// yet, so passing the receipt forward is the only way to tie the two together.
func WithAuditReceipt(ctx context.Context, receipt string) context.Context {
	if receipt == "" {
		return ctx
	}
	return context.WithValue(ctx, receiptKey{}, receipt)
}

// AuditReceiptFrom returns the audit receipt carried on ctx, empty when the work was not started by
// a recorded request.
func AuditReceiptFrom(ctx context.Context) string {
	receipt, _ := ctx.Value(receiptKey{}).(string)
	return receipt
}
