package run

import (
	"context"
	"time"
)

// Budgets keeps fixed-window allowances in the store, so every process sharing it spends from one
// allowance. A limit kept in each process's memory is a limit per process: behind a load balancer a
// caller gets the whole allowance again from every replica, and again after every restart. The
// database stores implement it, and so does the in-memory store, for processes that share one.
type Budgets interface {
	// SpendBudget records one use of the allowance named key and returns how many uses its current
	// window holds, this one included. A window opens at the first use after the previous window
	// closed and lasts window, measured on now.
	SpendBudget(ctx context.Context, key string, window time.Duration, now time.Time) (int, error)
	// BudgetSpent returns how many uses the window of key open at now holds, zero when none is.
	BudgetSpent(ctx context.Context, key string, now time.Time) (int, error)
}

// maxMemBudgets bounds how many windows the in-memory store keeps before it drops closed ones, so a
// sweep across addresses cannot grow it without end.
const maxMemBudgets = 4096

// budgetWindow is one allowance's open window in the in-memory store.
type budgetWindow struct {
	// ends is when the window closes. A use at ends still counts against it.
	ends time.Time
	// spent is how many uses the window holds.
	spent int
}

// SpendBudget records one use of key's allowance in the window open at now, opening one when none
// is, and returns how many uses that window holds.
func (m *memStore) SpendBudget(_ context.Context, key string, window time.Duration,
	now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.budgets == nil {
		m.budgets = make(map[string]*budgetWindow)
	}
	w, ok := m.budgets[key]
	if !ok || now.After(w.ends) {
		if len(m.budgets) >= maxMemBudgets {
			for k, old := range m.budgets {
				if now.After(old.ends) {
					delete(m.budgets, k)
				}
			}
		}
		m.budgets[key] = &budgetWindow{ends: now.Add(window), spent: 1}
		return 1, nil
	}
	w.spent++
	return w.spent, nil
}

// BudgetSpent returns how many uses key's window open at now holds.
func (m *memStore) BudgetSpent(_ context.Context, key string, now time.Time) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	w, ok := m.budgets[key]
	if !ok || now.After(w.ends) {
		return 0, nil
	}
	return w.spent, nil
}
