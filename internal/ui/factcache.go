package ui

// WithFactCacheAdminOnly tells the inventories page whether reading cached facts is restricted to
// admins, so it offers them only to who may read them. The server enforces the rule either way.
func WithFactCacheAdminOnly(adminOnly bool) Option {
	return func(u *UI) { u.factCacheAdminOnly = adminOnly }
}
