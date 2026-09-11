package natsprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/nats-io/nats.go"
)

// Provider wires the canonical contract interfaces to a NATS connection.
type Provider struct {
	cfg Config
	nc  *nats.Conn
	js  nats.JetStreamContext
}

// Diagnostics summarizes connection and stream health for operator visibility.
type Diagnostics struct {
	Connected     bool   `json:"connected"`
	ServerURL     string `json:"server_url,omitempty"`
	StreamName    string `json:"stream_name,omitempty"`
	Retention     string `json:"retention,omitempty"`
	StreamMsgs    uint64 `json:"stream_msgs,omitempty"`
	StreamBytes   uint64 `json:"stream_bytes,omitempty"`
	ConsumerCount int    `json:"consumer_count,omitempty"`
	LastError     string `json:"last_error,omitempty"`
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
	cfg := &nats.StreamConfig{
		Name:       p.cfg.StreamName,
		Subjects:   []string{p.cfg.SubjectPrefix + ".>"},
		Retention:  nats.LimitsPolicy,
		Storage:    nats.MemoryStorage,
		Discard:    nats.DiscardOld,
		Duplicates: 2 * time.Minute,
	}
	_, err := p.js.AddStream(cfg, nats.Context(ctx))
	if err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("natsprovider: bootstrap stream: %w", err)
	}
	return nil
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
	d.StreamMsgs = info.State.Msgs
	d.StreamBytes = info.State.Bytes
	d.ConsumerCount = int(info.State.Consumers)
	return d, nil
}

// Publish implements contract.EventPublisher.
func (p *Provider) Publish(ctx context.Context, event contract.EventEnvelope) error {
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
	ch := make(chan *nats.Msg, 256)
	sub, err := p.nc.ChanSubscribe(subject, ch)
	if err != nil {
		return nil, nil, err
	}
	var once sync.Once
	drain := func() {
		once.Do(func() {
			_ = sub.Unsubscribe()
			close(ch)
		})
	}
	go func() {
		<-ctx.Done()
		drain()
	}()
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
