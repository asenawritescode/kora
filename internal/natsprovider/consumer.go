package natsprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/nats-io/nats.go"
)

// Consumer provides durable pull-based delivery with dead-letter handling.
type Consumer struct {
	p            *Provider
	cfg          Config
	subMu        sync.Mutex
	runMu        sync.Mutex
	sub          *nats.Subscription
	seen         chan struct{}
	termDelivery func(*nats.Msg) error
}

// NewConsumer creates a durable JetStream consumer against the provider stream.
func NewConsumer(p *Provider, cfg Config) (*Consumer, error) {
	if p == nil {
		return nil, fmt.Errorf("natsprovider: provider is nil")
	}
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.MaxDeliver <= 0 {
		cfg.MaxDeliver = 5
	}
	// JetStream durable names are identifiers, not subjects. Tenant hostnames
	// commonly contain dots, so normalize them before creating consumers.
	cfg.ConsumerName = durableName(cfg.ConsumerName)
	return &Consumer{p: p, cfg: cfg, seen: make(chan struct{}, 1), termDelivery: func(msg *nats.Msg) error { return msg.Term() }}, nil
}

func durableName(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// Run consumes messages until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context, handler contract.Handler) error {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	filterSubject := c.cfg.EffectiveConsumerSubject()
	ackWait := c.cfg.AckWait
	if ackWait <= 0 {
		ackWait = 30 * time.Second
	}
	consumerCfg := &nats.ConsumerConfig{
		Durable:       c.cfg.ConsumerName,
		AckPolicy:     nats.AckExplicitPolicy,
		DeliverPolicy: nats.DeliverAllPolicy,
		FilterSubject: filterSubject,
		// The application applies MaxDeliver so RetryableError can preserve
		// capacity-rejected work beyond the ordinary failure limit.
		MaxDeliver: -1,
		AckWait:    ackWait,
	}
	_, err := c.p.js.AddConsumer(c.cfg.StreamName, consumerCfg, nats.Context(ctx))
	if err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("natsprovider: add consumer: %w", err)
	}

	sub, err := c.p.js.PullSubscribe(filterSubject, c.cfg.ConsumerName, nats.BindStream(c.cfg.StreamName))
	if err != nil {
		return fmt.Errorf("natsprovider: pull subscribe: %w", err)
	}
	c.subMu.Lock()
	c.sub = sub
	c.subMu.Unlock()
	defer func() {
		_ = sub.Unsubscribe()
		c.subMu.Lock()
		if c.sub == sub {
			c.sub = nil
		}
		c.subMu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		msgs, err := sub.Fetch(1, nats.MaxWait(250*time.Millisecond))
		if err != nil {
			if err == nats.ErrTimeout {
				continue
			}
			return err
		}
		for _, msg := range msgs {
			attempt := 1
			if md, metaErr := msg.Metadata(); metaErr == nil && md != nil {
				attempt = int(md.NumDelivered)
			}
			delivery := contract.Delivery{
				ID:      msg.Header.Get("Nats-Msg-Id"),
				Type:    msg.Subject,
				Site:    "",
				Data:    append([]byte(nil), msg.Data...),
				Attempt: attempt,
			}
			done := make(chan struct{})
			heartbeatDone := make(chan struct{})
			heartbeatErr := make(chan error, 1)
			heartbeatInterval := ackWait / 2
			if heartbeatInterval <= 0 {
				heartbeatInterval = ackWait
			}
			go func() {
				defer close(heartbeatDone)
				ticker := time.NewTicker(heartbeatInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						if err := msg.InProgress(); err != nil {
							heartbeatErr <- fmt.Errorf("natsprovider: extend delivery lease: %w", err)
							return
						}
					case <-done:
						return
					}
				}
			}()
			handlerErr := handler(ctx, delivery)
			close(done)
			<-heartbeatDone
			if ctx.Err() != nil {
				// Shutdown is not a delivery failure. Leave the message unsettled
				// so JetStream redelivers it to the next live worker.
				continue
			}
			select {
			case err := <-heartbeatErr:
				if handlerErr == nil {
					return err
				}
				handlerErr = errors.Join(handlerErr, err)
			default:
			}
			if handlerErr != nil {
				if delay, retryable := retryableDelay(handlerErr); retryable {
					if err := msg.NakWithDelay(delay); err != nil {
						return fmt.Errorf("natsprovider: negatively acknowledge retryable delivery: %w", err)
					}
					continue
				}
				if attempt >= c.cfg.MaxDeliver {
					if err := c.publishDeadLetter(ctx, delivery, handlerErr, msg); err != nil {
						return fmt.Errorf("natsprovider: publish dead-letter for delivery %q: %w", delivery.ID, err)
					}
					if err := c.termDelivery(msg); err != nil {
						return fmt.Errorf("natsprovider: terminate dead-lettered delivery %q: %w", delivery.ID, err)
					}
					continue
				}
				if err := msg.Nak(); err != nil {
					return fmt.Errorf("natsprovider: negatively acknowledge delivery %q: %w", delivery.ID, err)
				}
				continue
			}
			ackCtx, cancelAck := context.WithTimeout(context.Background(), 2*time.Second)
			err := msg.AckSync(nats.Context(ackCtx))
			cancelAck()
			if err != nil {
				return fmt.Errorf("natsprovider: acknowledge delivery %q: %w", delivery.ID, err)
			}
		}
	}
}

// Drain stops delivery and drains the subscription if it exists.
func (c *Consumer) Drain(ctx context.Context) error {
	if ctx == nil {
		return errors.New("natsprovider: drain context is required")
	}
	c.subMu.Lock()
	sub := c.sub
	c.subMu.Unlock()
	if sub == nil {
		return nil
	}
	return drainSubscription(ctx, sub.Drain, sub.Unsubscribe)
}

// drainSubscription bounds graceful drain, then force-unsubscribes and joins
// the drain goroutine before returning. Callers may close the provider only
// after this function returns.
func drainSubscription(ctx context.Context, drain, unsubscribe func() error) error {
	if ctx == nil {
		return errors.New("natsprovider: drain context is required")
	}
	done := make(chan error, 1)
	go func() { done <- drain() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		unsubscribeErr := unsubscribe()
		drainErr := <-done
		return errors.Join(unsubscribeErr, drainErr)
	}
}

func (c *Consumer) publishDeadLetter(ctx context.Context, delivery contract.Delivery, cause error, original *nats.Msg) error {
	payload, _ := json.Marshal(map[string]any{
		"delivery": delivery,
		"error":    cause.Error(),
	})
	message := &nats.Msg{Subject: c.cfg.DeadLetterSubject, Data: payload}
	messageID := delivery.ID
	if messageID == "" && original != nil {
		if md, err := original.Metadata(); err == nil && md != nil {
			messageID = fmt.Sprintf("%s:%d", md.Stream, md.Sequence.Stream)
		}
	}
	options := []nats.PubOpt{nats.Context(ctx)}
	if messageID != "" {
		options = append(options, nats.MsgId("dead-letter:"+messageID))
	}
	_, err := c.p.js.PublishMsg(message, options...)
	return err
}
