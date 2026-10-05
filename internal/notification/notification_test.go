package notification_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/notificationtest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	notificationtest.Contract(t, notification.NewMemStore)
}
