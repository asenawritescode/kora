package engine

import (
	"context"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/analytics"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/notifications"
)

func TestInboxNotificationSenderPublishesRecipientScopedInvalidationOnly(t *testing.T) {
	bus := analytics.NewChannelBusWithLimits(2, 2048, 4096, t.TempDir())
	defer bus.Close()
	events, err := bus.Subscribe()
	if err != nil {
		t.Fatal("subscribe to local event bus:", err)
	}
	sender := inboxNotificationSender{site: "tenant-route", bus: bus}
	_, err = sender.Send(context.Background(), notifications.Delivery{
		ID: "01K7NOTIFICATION000000000000", Tenant: "stable-tenant", Recipient: "bob", RequestID: "01K7REQUEST000000000000000000",
		Channel: notifications.ChannelInbox, EventID: "01K7REQUESTEVENT0000000000000",
	})
	if err != nil {
		t.Fatal("send in-app notification:", err)
	}
	select {
	case event := <-events:
		if event.ID != "01K7NOTIFICATION000000000000" || event.Site != "tenant-route" || event.Operation != eventstream.EventNotification {
			t.Fatalf("notification invalidation envelope = %#v", event)
		}
		if event.Data["recipient"] != "bob" || event.Data["request_id"] != "01K7REQUEST000000000000000000" || event.Data["event_id"] != "01K7REQUESTEVENT0000000000000" {
			t.Fatalf("notification invalidation recipient/request scope = %#v", event.Data)
		}
		if len(event.Data) != 3 {
			t.Fatalf("notification contained unexpected content: %#v", event.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("recipient-scoped notification was not published")
	}
	if _, err := sender.Send(context.Background(), notifications.Delivery{Tenant: contract.TenantID("stable-tenant")}); err == nil {
		t.Fatal("invalid notification delivery was accepted")
	}
	if _, err := sender.Send(context.Background(), notifications.Delivery{ID: "01K7NOTIFICATION000000000001", Tenant: "stable-tenant", Recipient: "bob", RequestID: "01K7REQUEST000000000000000000", Channel: notifications.ChannelEmail}); err == nil {
		t.Fatal("email delivery was incorrectly sent through the in-app channel")
	}
}
