// Package kafkatopics checks that Kafka topics exist. Infra owns topic
// creation, so the services only detect, report and wait for missing topics;
// they never create them.
package kafkatopics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"
)

// Wait polling policy.
const (
	waitInitialBackoff = time.Second
	waitMaxBackoff     = 30 * time.Second
	// checkTimeout bounds one metadata request, so an unreachable broker
	// cannot stall a wait iteration indefinitely.
	checkTimeout = 10 * time.Second
)

// Missing returns the topics, in the given order, that don't exist on the
// cluster. A topic is missing when the metadata response omits it or reports
// UnknownTopicOrPartition for it. Any other topic error is returned.
//
// The metadata request never asks the broker to auto-create topics.
func Missing(ctx context.Context, brokers []string, topics ...string) ([]string, error) {
	client := &kafka.Client{Addr: kafka.TCP(brokers...)}
	res, err := client.Metadata(ctx, &kafka.MetadataRequest{Topics: topics})
	if err != nil {
		return nil, fmt.Errorf("kafka metadata: %w", err)
	}

	found := make(map[string]bool, len(res.Topics))
	for _, t := range res.Topics {
		switch {
		case t.Error == nil:
			found[t.Name] = true
		case errors.Is(t.Error, kafka.UnknownTopicOrPartition):
			// missing
		default:
			return nil, fmt.Errorf("kafka metadata for topic %s: %w", t.Name, t.Error)
		}
	}

	var missing []string
	for _, t := range topics {
		if !found[t] {
			missing = append(missing, t)
		}
	}
	return missing, nil
}

// Wait blocks until every topic exists. It polls with backoff from 1 s,
// doubling up to 30 s, and logs an ERROR naming the missing topics (or the
// metadata error) on each attempt. It returns nil once all topics exist, or
// ctx.Err() when ctx is done first.
func Wait(ctx context.Context, brokers []string, topics ...string) error {
	check := func(ctx context.Context) ([]string, error) {
		ctx, cancel := context.WithTimeout(ctx, checkTimeout)
		defer cancel()
		return Missing(ctx, brokers, topics...)
	}
	return wait(ctx, topics, check, sleepContext)
}

func wait(
	ctx context.Context,
	topics []string,
	check func(ctx context.Context) ([]string, error),
	sleep func(ctx context.Context, d time.Duration) error,
) error {
	delay := waitInitialBackoff
	for {
		missing, err := check(ctx)
		if err == nil && len(missing) == 0 {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			slog.Error("cannot check kafka topics, retrying",
				"topics", topics, "error", err, "retry_in", delay)
		} else {
			slog.Error("kafka topics missing, waiting for infra to create them",
				"missing", missing, "retry_in", delay)
		}
		if err := sleep(ctx, delay); err != nil {
			return err
		}
		delay = min(delay*2, waitMaxBackoff)
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Checker reports missing topics on a fixed set of brokers. It satisfies
// consumer-side interfaces such as the API's readiness check.
type Checker struct {
	Brokers []string
}

// Missing returns the topics that don't exist on c.Brokers.
func (c Checker) Missing(ctx context.Context, topics ...string) ([]string, error) {
	return Missing(ctx, c.Brokers, topics...)
}
