package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/eventstream"
	"github.com/asenawritescode/kora/internal/tenantruntime"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/notifications"
	notificationsql "github.com/asenawritescode/kora/notifications/sqlstore"
	"github.com/asenawritescode/kora/outbox"
	requestsql "github.com/asenawritescode/kora/requests/sqlstore"
)

type requestNotificationPlanner struct {
	store  *requestsql.Store
	tenant contract.TenantID
	site   string
}

func (p requestNotificationPlanner) Plan(ctx context.Context, event contract.EventEnvelope) ([]notifications.PlannedDelivery, error) {
	if p.store == nil || p.tenant == "" || p.site == "" {
		return nil, errors.New("request notification planner is not tenant-scoped")
	}
	if event.AggregateType != "request" {
		return nil, contract.NewError(contract.CodeValidationFailed, "notification event does not identify a request")
	}
	requestID, err := contract.ParseRequestID(event.AggregateID)
	if err != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, "notification request identity is invalid")
	}
	recipients, err := p.store.DirectTaskNotificationRecipients(ctx, p.tenant, p.site, requestID, event.Type)
	if err != nil {
		return nil, err
	}
	planned := make([]notifications.PlannedDelivery, 0, len(recipients))
	for _, recipient := range recipients {
		planned = append(planned, notifications.PlannedDelivery{Recipient: recipient, RequestID: requestID, Channel: notifications.ChannelInbox})
	}
	return planned, nil
}

type inboxNotificationSender struct {
	site string
	bus  analytics.EventBus
}

func (s inboxNotificationSender) Send(ctx context.Context, delivery notifications.Delivery) (notifications.DeliveryResult, error) {
	if ctx == nil || s.site == "" || s.bus == nil || delivery.ID == "" || delivery.Tenant == "" || delivery.Recipient == "" || delivery.RequestID == "" || delivery.Channel != notifications.ChannelInbox {
		return notifications.DeliveryResult{}, errors.New("in-app notification is not tenant-scoped")
	}
	if err := ctx.Err(); err != nil {
		return notifications.DeliveryResult{}, err
	}
	err := s.bus.Publish(eventstream.ChangeEvent{
		ID: string(delivery.ID), Site: s.site, Operation: eventstream.EventNotification, Timestamp: time.Now().UTC(),
		Data: map[string]any{"recipient": string(delivery.Recipient), "request_id": string(delivery.RequestID), "event_id": delivery.EventID},
	})
	if err != nil {
		return notifications.DeliveryResult{}, err
	}
	return notifications.DeliveryResult{ProviderReference: string(delivery.ID)}, nil
}

func registerRequestNotifications(config TenantServiceConfig, loaded *knet.LoadedSite, services *tenantruntime.Services, requestStore *requestsql.Store, dialect kdb.Dialect) error {
	if loaded == nil || services == nil || requestStore == nil || services.EventBus == nil || services.WorkAdmission == nil {
		return errors.New("request notifications require a tenant runtime, request store, event bus and work admission")
	}
	tenant, err := contract.ParseTenantID(loaded.SiteID)
	if err != nil {
		return fmt.Errorf("request notification tenant identity: %w", err)
	}
	store, err := notificationsql.New(loaded.DB, dialect)
	if err != nil {
		return fmt.Errorf("construct notification store: %w", err)
	}
	service, err := notifications.NewService(tenant, store, requestNotificationPlanner{store: requestStore, tenant: tenant, site: loaded.Name}, requestWallClock{}, requestIDs{})
	if err != nil {
		return fmt.Errorf("construct request notification service: %w", err)
	}
	dispatcher := outbox.NewDispatcher(loaded.DB, dialect)
	dispatcher.WorkAdmission = services.WorkAdmission
	if err := dispatcher.RegisterForTypes("request-notifications", []string{
		"request.created", "request.responded", "request.reassigned", "request.cancelled", "request.expired", "request.stale",
	}, func(ctx context.Context, event contract.EventEnvelope) error {
		release, err := services.WorkAdmission.Acquire(ctx)
		if err != nil {
			return err
		}
		defer release()
		return service.Consume(ctx, event)
	}); err != nil {
		return fmt.Errorf("register request notification outbox consumer: %w", err)
	}
	worker, err := notifications.NewWorkerWithAdmission(tenant, store, inboxNotificationSender{site: loaded.Name, bus: services.EventBus}, requestWallClock{}, contract.NewID(), services.WorkAdmission)
	if err != nil {
		return fmt.Errorf("construct request notification worker: %w", err)
	}
	if err := registerTenantWorkerStart(config, func() error {
		if !loaded.StartWorker(config.ProcessContext, func(ctx context.Context) {
			RunRequestNotificationDispatcher(ctx, dispatcher)
		}) {
			return errors.New("tenant runtime is stopping")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("register request notification event consumer: %w", err)
	}
	if err := registerTenantWorkerStart(config, func() error {
		if !loaded.StartWorker(config.ProcessContext, func(ctx context.Context) {
			RunRequestNotificationWorker(ctx, worker)
		}) {
			return errors.New("tenant runtime is stopping")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("register request notification delivery worker: %w", err)
	}
	return nil
}
