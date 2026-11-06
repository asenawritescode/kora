package natsprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/nats-io/nats.go"
)

// Provider wires the canonical contract interfaces to a NATS connection.
type Provider struct {
	cfg                Config
	nc                 *nats.Conn
	js                 nats.JetStreamContext
	eventStreamDurable atomic.Bool
	realtimeMu         sync.RWMutex
	realtimeSubs       map[*nats.Subscription]*realtimeSubscriptionState
	realtimeDropped    atomic.Int64
}

type realtimeSubscriptionState struct {
	queue                      chan *nats.Msg
	oldestQueuedAt             time.Time
	clientPendingObservedSince time.Time
}

const (
	realtimeSubscriptionQueueItems = 8
	realtimeSubscriptionQueueBytes = 8 << 20
	realtimeSubscriptionMaxMessage = 1 << 20
)

// RealtimeBudgetSnapshot reports process-local NATS and application queue
// occupancy for active realtime subscriptions. Application bytes are a
// conservative bound based on the enforced per-message payload limit.
type RealtimeBudgetSnapshot struct {
	Subscriptions             int
	ClientPendingMessages     int
	ClientPendingBytes        int
	ApplicationPendingItems   int
	ApplicationPendingBytes   int
	ApplicationCapacityItems  int
	ApplicationCapacityBytes  int
	ClientDroppedMessages     int
	ApplicationOldestAge      time.Duration
	ClientPendingNonemptySpan time.Duration
}

func (p *Provider) RealtimeBudget() RealtimeBudgetSnapshot {
	if p == nil {
		return RealtimeBudgetSnapshot{}
	}
	p.realtimeMu.Lock()
	defer p.realtimeMu.Unlock()
	snapshot := RealtimeBudgetSnapshot{Subscriptions: len(p.realtimeSubs), ClientDroppedMessages: int(p.realtimeDropped.Load())}
	snapshot.ApplicationCapacityItems = len(p.realtimeSubs) * realtimeSubscriptionQueueItems
	snapshot.ApplicationCapacityBytes = len(p.realtimeSubs) * realtimeSubscriptionQueueItems * realtimeSubscriptionMaxMessage
	now := time.Now().UTC()
	for sub, state := range p.realtimeSubs {
		queue := state.queue
		snapshot.ApplicationPendingItems += len(queue)
		snapshot.ApplicationPendingBytes += len(queue) * realtimeSubscriptionMaxMessage
		if len(queue) == 0 {
			state.oldestQueuedAt = time.Time{}
		} else {
			if state.oldestQueuedAt.IsZero() {
				state.oldestQueuedAt = now
			}
			age := now.Sub(state.oldestQueuedAt)
			if age < 0 {
				age = 0
			}
			if age > snapshot.ApplicationOldestAge {
				snapshot.ApplicationOldestAge = age
			}
		}
		messages, bytes, err := sub.Pending()
		if err == nil {
			snapshot.ClientPendingMessages += messages
			snapshot.ClientPendingBytes += bytes
			span := updatePendingNonemptySpan(&state.clientPendingObservedSince, messages, now)
			if span > snapshot.ClientPendingNonemptySpan {
				snapshot.ClientPendingNonemptySpan = span
			}
		} else {
			// An unsuccessful sample breaks the evidence that the backlog stayed
			// nonempty between observations.
			state.clientPendingObservedSince = time.Time{}
		}
		if dropped, err := sub.Dropped(); err == nil {
			snapshot.ClientDroppedMessages += dropped
		}
	}
	return snapshot
}

// updatePendingNonemptySpan tracks the sampled span during which a client
// backlog was observed nonempty. Core NATS does not expose each buffered
// message's enqueue time, so this is not an estimate of message age.
func updatePendingNonemptySpan(since *time.Time, pending int, now time.Time) time.Duration {
	if since == nil {
		return 0
	}
	if pending <= 0 {
		*since = time.Time{}
		return 0
	}
	if since.IsZero() {
		*since = now
	}
	age := now.Sub(*since)
	if age < 0 {
		return 0
	}
	return age
}

func (p *Provider) trackRealtimeSubscription(sub *nats.Subscription, queue chan *nats.Msg) {
	p.realtimeMu.Lock()
	if p.realtimeSubs == nil {
		p.realtimeSubs = make(map[*nats.Subscription]*realtimeSubscriptionState)
	}
	p.realtimeSubs[sub] = &realtimeSubscriptionState{queue: queue}
	p.realtimeMu.Unlock()
}

func (p *Provider) recordRealtimeEnqueue(sub *nats.Subscription) {
	p.realtimeMu.Lock()
	defer p.realtimeMu.Unlock()
	state := p.realtimeSubs[sub]
	if state == nil {
		return
	}
	if state.oldestQueuedAt.IsZero() {
		state.oldestQueuedAt = time.Now().UTC()
	}
}

func (p *Provider) untrackRealtimeSubscription(sub *nats.Subscription) {
	p.realtimeMu.Lock()
	defer p.realtimeMu.Unlock()
	if dropped, err := sub.Dropped(); err == nil && dropped > 0 {
		p.realtimeDropped.Add(int64(dropped))
	}
	delete(p.realtimeSubs, sub)
}

// Diagnostics summarizes connection and stream health for operator visibility.
type Diagnostics struct {
	Connected      bool   `json:"connected"`
	ServerURL      string `json:"server_url,omitempty"`
	StreamName     string `json:"stream_name,omitempty"`
	StreamStorage  string `json:"stream_storage,omitempty"`
	Retention      string `json:"retention,omitempty"`
	StreamMsgs     uint64 `json:"stream_msgs,omitempty"`
	StreamBytes    uint64 `json:"stream_bytes,omitempty"`
	StreamMaxMsgs  uint64 `json:"stream_max_msgs,omitempty"`
	StreamMaxBytes uint64 `json:"stream_max_bytes,omitempty"`
	ConsumerCount  int    `json:"consumer_count,omitempty"`
	LastError      string `json:"last_error,omitempty"`
}

// Config returns a copy of the provider configuration.
func (p *Provider) Config() Config {
	if p == nil {
		return Config{}
	}
	return p.cfg
}

// New connects to NATS and returns a provider implementing the kernel contracts.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	opts := []nats.Option{nats.Name(cfg.Name)}
	if cfg.Username != "" || cfg.Password != "" {
		opts = append(opts, nats.UserInfo(cfg.Username, cfg.Password))
	}
	if cfg.Token != "" {
		opts = append(opts, nats.Token(cfg.Token))
	}
	opts = append(opts, nats.Timeout(5*time.Second))

	nc, err := nats.Connect(stringsJoin(cfg.ServerURLs), opts...)
	if err != nil {
		return nil, fmt.Errorf("natsprovider: connect: %w", err)
	}

	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("natsprovider: jetstream: %w", err)
	}

	return &Provider{cfg: cfg, nc: nc, js: js}, nil
}

// Close closes the underlying connection.
func (p *Provider) Close() {
	if p.nc != nil {
		p.nc.Close()
	}
}

// Bootstrap ensures the configured stream exists. It is idempotent.
func (p *Provider) Bootstrap(ctx context.Context) error {
	p.eventStreamDurable.Store(false)
	cfg := &nats.StreamConfig{
		Name:       p.cfg.StreamName,
		Subjects:   p.eventStreamSubjects(),
		Retention:  nats.LimitsPolicy,
		Storage:    nats.FileStorage,
		Discard:    nats.DiscardNew,
		MaxBytes:   p.cfg.StreamMaxBytes,
		MaxMsgs:    int64(p.cfg.StreamMaxMessages),
		Duplicates: 2 * time.Minute,
	}
	if _, err := p.js.AddStream(cfg, nats.Context(ctx)); err != nil {
		if !isAlreadyExists(err) {
			return fmt.Errorf("natsprovider: bootstrap stream: %w", err)
		}
		existing, infoErr := p.js.StreamInfo(cfg.Name, nats.Context(ctx))
		if infoErr != nil {
			return fmt.Errorf("natsprovider: inspect existing event stream before applying limits: %w", infoErr)
		}
		if !subjectsWithin(existing.Config.Subjects, cfg.Subjects) || !slices.Contains(existing.Config.Subjects, cfg.Subjects[0]) || existing.Config.Retention != nats.LimitsPolicy {
			return fmt.Errorf("natsprovider: existing event stream %q has incompatible subjects or retention", cfg.Name)
		}
		if existing.Config.MaxAge > 0 {
			return fmt.Errorf("natsprovider: existing event stream %q has age-based expiry; migrate it before enabling durable bounded delivery", cfg.Name)
		}
		if existing.Config.Storage == nats.MemoryStorage {
			// JetStream does not allow changing a stream's storage type in place.
			// Keep an existing memory stream operational during the migration
			// window, but report its storage mode so operators do not mistake its
			// publish acknowledgments for restart-durable acceptance. New streams
			// use bounded file storage.
			cfg.Storage = existing.Config.Storage
		}
		if existing.State.Msgs > uint64(cfg.MaxMsgs) || existing.State.Bytes > uint64(cfg.MaxBytes) {
			return fmt.Errorf("natsprovider: existing event stream %q exceeds the configured caps; migrate or drain it before lowering limits", cfg.Name)
		}
		updated := existing.Config
		// Streams from earlier releases capture only "<prefix>.>"; add the
		// dead-letter subject so dead letters are stored.
		updated.Subjects = cfg.Subjects
		updated.MaxBytes = cfg.MaxBytes
		updated.MaxMsgs = cfg.MaxMsgs
		updated.Discard = nats.DiscardNew
		if _, err := p.js.UpdateStream(&updated, nats.Context(ctx)); err != nil {
			return fmt.Errorf("natsprovider: apply bounded stream configuration: %w", err)
		}
	}
	info, err := p.js.StreamInfo(cfg.Name, nats.Context(ctx))
	if err != nil {
		return fmt.Errorf("natsprovider: inspect bounded event stream: %w", err)
	}
	if info.Config.MaxBytes <= 0 || info.Config.MaxMsgs <= 0 || info.Config.Discard != nats.DiscardNew || info.Config.MaxAge > 0 || (info.Config.Storage != nats.FileStorage && info.Config.Storage != nats.MemoryStorage) {
		return fmt.Errorf("natsprovider: event stream %q is not bounded with discard-new behavior", cfg.Name)
	}
	p.eventStreamDurable.Store(info.Config.Storage == nats.FileStorage)
	return nil
}

// eventStreamSubjects is the event tree plus, when it lies outside that tree,
// the dead-letter subject, so the stream stores dead letters that no consumer
// filter on the event tree can match.
func (p *Provider) eventStreamSubjects() []string {
	tree := p.cfg.SubjectPrefix + ".>"
	subjects := []string{tree}
	if dead := p.cfg.withDefaults().DeadLetterSubject; dead != "" && !subjectMatchesFilter(tree, dead) {
		subjects = append(subjects, dead)
	}
	return subjects
}

func subjectsWithin(have, allowed []string) bool {
	for _, subject := range have {
		if !slices.Contains(allowed, subject) {
			return false
		}
	}
	return true
}

func (p *Provider) BootstrapSiteDirectory(ctx context.Context) error {
	if p == nil || p.js == nil {
		return fmt.Errorf("natsprovider: provider is nil")
	}
	name := p.SiteDirectoryStreamName()
	cfg := &nats.StreamConfig{
		Name: name, Subjects: []string{p.cfg.SubjectPrefix + "_site_directory.>"},
		Retention: nats.LimitsPolicy, Storage: nats.FileStorage,
		Discard: nats.DiscardOld, MaxAge: 24 * time.Hour,
		MaxBytes: DirectoryStreamMaxBytes, MaxMsgs: DirectoryStreamMaxMessages,
		Duplicates: 10 * time.Minute,
	}
	if _, err := p.js.AddStream(cfg, nats.Context(ctx)); err != nil {
		if !isAlreadyExists(err) {
			return fmt.Errorf("natsprovider: bootstrap site directory stream: %w", err)
		}
		existing, infoErr := p.js.StreamInfo(name, nats.Context(ctx))
		if infoErr != nil {
			return fmt.Errorf("natsprovider: inspect existing site directory stream before applying limits: %w", infoErr)
		}
		if len(existing.Config.Subjects) != 1 || existing.Config.Subjects[0] != cfg.Subjects[0] || existing.Config.Retention != nats.LimitsPolicy {
			return fmt.Errorf("natsprovider: existing site directory stream %q has incompatible subjects or retention", name)
		}
		updated := existing.Config
		updated.MaxBytes = cfg.MaxBytes
		updated.MaxMsgs = cfg.MaxMsgs
		updated.MaxAge = cfg.MaxAge
		updated.Discard = cfg.Discard
		if _, err := p.js.UpdateStream(&updated, nats.Context(ctx)); err != nil {
			return fmt.Errorf("natsprovider: apply bounded site directory stream configuration: %w", err)
		}
	}
	info, err := p.js.StreamInfo(name, nats.Context(ctx))
	if err != nil {
		return fmt.Errorf("natsprovider: inspect site directory stream: %w", err)
	}
	if len(info.Config.Subjects) != 1 || info.Config.Subjects[0] != cfg.Subjects[0] || info.Config.MaxBytes <= 0 || info.Config.MaxMsgs <= 0 {
		return fmt.Errorf("natsprovider: site directory stream %q has incompatible subjects or missing resource limits", name)
	}
	return nil
}

// SiteDirectoryStreamName and SiteDirectorySubject are deliberately outside
// the general event subject tree so control-plane notifications have separate
// retention and cannot be confused with analytics or conversation events.
func (p *Provider) SiteDirectoryStreamName() string {
	return strings.ToUpper(p.cfg.SubjectPrefix) + "_SITE_DIRECTORY"
}

func (p *Provider) SiteDirectorySubject() string {
	return p.cfg.SubjectPrefix + "_site_directory.changed"
}

type SiteDirectoryWakeup struct {
	Version           int    `json:"version"`
	DirectoryRevision uint64 `json:"directory_revision"`
}

// PublishSiteDirectoryRevision emits a small, credential-free wakeup. Message
// IDs make duplicate publishers idempotent; consumers always read the SQL feed.
func (p *Provider) PublishSiteDirectoryRevision(ctx context.Context, revision uint64) error {
	if p == nil || p.js == nil || revision == 0 {
		return fmt.Errorf("natsprovider: provider and positive directory revision are required")
	}
	payload, err := json.Marshal(SiteDirectoryWakeup{Version: 1, DirectoryRevision: revision})
	if err != nil {
		return err
	}
	_, err = p.js.PublishMsg(&nats.Msg{Subject: p.SiteDirectorySubject(), Data: payload},
		nats.Context(ctx), nats.MsgId(fmt.Sprintf("site-directory-revision-%d", revision)))
	if err != nil {
		return fmt.Errorf("natsprovider: publish site directory revision %d: %w", revision, err)
	}
	return nil
}

// SubscribeSiteDirectoryWakeups is a durable per-engine JetStream consumer.
// SQL polling remains the recovery mechanism if NATS retention expires.
func (p *Provider) SubscribeSiteDirectoryWakeups(ctx context.Context, consumerID string) (<-chan uint64, error) {
	if p == nil || p.js == nil || strings.TrimSpace(consumerID) == "" {
		return nil, fmt.Errorf("natsprovider: provider and consumer identity are required")
	}
	digest := sha256.Sum256([]byte(consumerID))
	durableName := "dir_" + hex.EncodeToString(digest[:12])
	// Older adapter builds could leave a push durable under this name. Such a
	// consumer cannot be rebound for pull delivery; recreate it. Wakeups are
	// hints only, so the SQL outbox poller safely recovers any skipped revision.
	if info, err := p.js.ConsumerInfo(p.SiteDirectoryStreamName(), durableName, nats.Context(ctx)); err == nil {
		if info.Config.DeliverSubject != "" {
			if err := p.js.DeleteConsumer(p.SiteDirectoryStreamName(), durableName, nats.Context(ctx)); err != nil {
				return nil, fmt.Errorf("natsprovider: replace incompatible site directory consumer: %w", err)
			}
		}
	} else if !errors.Is(err, nats.ErrConsumerNotFound) {
		return nil, fmt.Errorf("natsprovider: inspect site directory consumer: %w", err)
	}
	subscription, err := p.js.PullSubscribe(p.SiteDirectorySubject(), durableName,
		nats.DeliverAll(), nats.AckExplicit(), nats.ManualAck(), nats.MaxAckPending(256))
	if err != nil {
		return nil, fmt.Errorf("natsprovider: pull subscribe: %w", err)
	}
	revisions := make(chan uint64, 256)
	go func() {
		defer close(revisions)
		defer subscription.Unsubscribe()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			messages, err := subscription.Fetch(1, nats.MaxWait(500*time.Millisecond))
			if err != nil {
				if errors.Is(err, nats.ErrTimeout) {
					continue
				}
				if ctx.Err() != nil {
					return
				}
				time.Sleep(200 * time.Millisecond)
				continue
			}
			for _, message := range messages {
				var wakeup SiteDirectoryWakeup
				if json.Unmarshal(message.Data, &wakeup) != nil || wakeup.Version != 1 || wakeup.DirectoryRevision == 0 {
					_ = message.Ack()
					continue
				}
				select {
				case revisions <- wakeup.DirectoryRevision:
					_ = message.Ack()
				case <-ctx.Done():
					_ = message.Nak()
					return
				}
			}
		}
	}()
	return revisions, nil
}

// Diagnostics reads JetStream state for the configured stream. It never mutates
// the provider and fails closed if stream metadata cannot be fetched.
func (p *Provider) Diagnostics(ctx context.Context) (Diagnostics, error) {
	if p == nil || p.nc == nil {
		return Diagnostics{}, fmt.Errorf("natsprovider: provider is nil")
	}
	d := Diagnostics{
		Connected:  p.nc.IsConnected(),
		ServerURL:  p.nc.ConnectedUrl(),
		StreamName: p.cfg.StreamName,
	}
	info, err := p.js.StreamInfo(p.cfg.StreamName, nats.Context(ctx))
	if err != nil {
		return d, fmt.Errorf("natsprovider: stream info: %w", err)
	}
	d.Retention = fmt.Sprintf("%v", info.Config.Retention)
	d.StreamStorage = fmt.Sprintf("%v", info.Config.Storage)
	d.StreamMsgs = info.State.Msgs
	d.StreamBytes = info.State.Bytes
	if info.Config.MaxMsgs > 0 {
		d.StreamMaxMsgs = uint64(info.Config.MaxMsgs)
	}
	if info.Config.MaxBytes > 0 {
		d.StreamMaxBytes = uint64(info.Config.MaxBytes)
	}
	d.ConsumerCount = int(info.State.Consumers)
	return d, nil
}

// Publish implements contract.EventPublisher.
func (p *Provider) Publish(ctx context.Context, event contract.EventEnvelope) error {
	if p == nil || p.js == nil {
		return errors.New("natsprovider: provider is nil")
	}
	if !p.eventStreamDurable.Load() {
		return ErrNonDurableEventStream
	}
	// Preserve the pre-envelope provider API for callers that supplied the
	// original minimal event shape. The normalized event is still validated
	// before it crosses the provider boundary.
	if event.Version == 0 {
		event.Version = contract.CurrentVersion
	}
	if event.Source == "" {
		event.Source = "natsprovider"
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if len(event.Data) == 0 {
		event.Data = json.RawMessage(`{}`)
	}
	if err := event.Validate(); err != nil {
		return fmt.Errorf("nats: validate event: %w", err)
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	subject := p.cfg.SubjectPrefix + "." + event.Type
	_, err = p.js.PublishMsg(&nats.Msg{Subject: subject, Data: data}, nats.Context(ctx), nats.MsgId(event.ID))
	return err
}

// ErrNonDurableEventStream prevents an outbox publish acknowledgment from
// being treated as durable when a deployed legacy stream uses memory storage.
// The outbox row remains available for retry after the stream migration.
var ErrNonDurableEventStream = errors.New("natsprovider: event stream has not been verified as restart-durable file storage")

// PublishSubject publishes raw bytes to an internal subject.
func (p *Provider) PublishSubject(ctx context.Context, subject string, data []byte, msgID string) error {
	if p == nil || p.nc == nil {
		return fmt.Errorf("natsprovider: provider is nil")
	}
	msg := &nats.Msg{Subject: subject, Data: data}
	if msgID == "" {
		return p.nc.PublishMsg(msg)
	}
	_, err := p.js.PublishMsg(msg, nats.Context(ctx), nats.MsgId(msgID))
	return err
}

// Request sends a synchronous command and decodes a CommandResult response.
func (p *Provider) Request(ctx context.Context, command contract.CommandEnvelope) (contract.CommandResult, error) {
	body, err := json.Marshal(command)
	if err != nil {
		return contract.CommandResult{}, err
	}
	msg, err := p.nc.Request(p.cfg.SubjectPrefix+".command."+command.Type, body, contextDeadline(ctx))
	if err != nil {
		return contract.CommandResult{}, err
	}
	var result contract.CommandResult
	if err := json.Unmarshal(msg.Data, &result); err != nil {
		return contract.CommandResult{}, err
	}
	return result, nil
}

// Submit sends an asynchronous command and returns an accepted receipt.
func (p *Provider) Submit(ctx context.Context, command contract.CommandEnvelope) (contract.TaskReceipt, error) {
	body, err := json.Marshal(command)
	if err != nil {
		return contract.TaskReceipt{}, err
	}
	if err := p.nc.Publish(p.cfg.SubjectPrefix+".task."+command.Type, body); err != nil {
		return contract.TaskReceipt{}, err
	}
	return contract.TaskReceipt{OperationID: command.ID, CorrelationID: command.CorrelationID, Status: contract.StatusAccepted, AcceptedAt: time.Now().UTC()}, nil
}

// Subscribe opens a NATS subscription for the given subject.
// The caller must cancel the context or close the returned drain function.
func (p *Provider) Subscribe(ctx context.Context, subject string) (<-chan *nats.Msg, func(), error) {
	if p == nil || p.nc == nil {
		return nil, nil, fmt.Errorf("natsprovider: provider is nil")
	}
	if ctx == nil {
		return nil, nil, fmt.Errorf("natsprovider: subscription context is required")
	}
	ch := make(chan *nats.Msg, realtimeSubscriptionQueueItems)
	sub, err := p.nc.SubscribeSync(subject)
	if err != nil {
		return nil, nil, err
	}
	if err := sub.SetPendingLimits(realtimeSubscriptionQueueItems, realtimeSubscriptionQueueBytes); err != nil {
		_ = sub.Unsubscribe()
		close(ch)
		return nil, nil, fmt.Errorf("natsprovider: set bounded subscription queue: %w", err)
	}
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopWatcher := func() { stopOnce.Do(func() { close(stop) }) }
	watcherDone := make(chan struct{})
	var unsubscribeOnce sync.Once
	unsubscribe := func() { unsubscribeOnce.Do(func() { _ = sub.Unsubscribe() }) }
	p.trackRealtimeSubscription(sub, ch)
	go func() {
		defer close(watcherDone)
		defer close(ch)
		defer unsubscribe()
		defer p.untrackRealtimeSubscription(sub)
		for {
			message, receiveErr := sub.NextMsg(100 * time.Millisecond)
			if receiveErr == nil {
				if len(message.Data) > realtimeSubscriptionMaxMessage {
					// The HTTP stream has a strict payload ceiling as well as bounded
					// queue capacity; clients refetch the full authorized state.
					return
				}
				select {
				case ch <- message:
					p.recordRealtimeEnqueue(sub)
				case <-ctx.Done():
					return
				case <-stop:
					return
				default:
					// The application queue is also bounded. Close the realtime
					// stream so the client reconnects and refetches authorized state.
					return
				}
			} else if !errors.Is(receiveErr, nats.ErrTimeout) {
				return
			}
			dropped, dropErr := sub.Dropped()
			if dropErr != nil || dropped > 0 {
				// Realtime delivery is a hint; overflow forces a reconnect/refetch.
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			default:
			}
		}
	}()
	drain := func() {
		unsubscribe()
		stopWatcher()
		<-watcherDone
	}
	return ch, drain, nil
}

func contextDeadline(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		d := time.Until(deadline)
		if d > 0 {
			return d
		}
	}
	return 5 * time.Second
}

func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "stream name already in use")
}

func stringsJoin(parts []string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts, ",")
}
