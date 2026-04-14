package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"authz-service/internal/model"

	"github.com/redis/go-redis/v9"
)

type Cache struct {
	client *redis.Client
	ttl    time.Duration
}

func NewCache(addr string, ttl time.Duration) *Cache {
	client := redis.NewClient(&redis.Options{
		Addr: addr,
	})
	return &Cache{client: client, ttl: ttl}
}

func (c *Cache) Client() *redis.Client {
	return c.client
}

func (c *Cache) checkKey(req model.CheckRequest) string {
	return fmt.Sprintf("authz:check:%s:%s:%s:%s",
		req.UserID, req.ResourceType, req.ResourceID, req.Action)
}

func (c *Cache) Get(ctx context.Context, req model.CheckRequest) (*model.CheckResult, error) {
	val, err := c.client.Get(ctx, c.checkKey(req)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result model.CheckResult
	if err := json.Unmarshal([]byte(val), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Cache) Set(ctx context.Context, req model.CheckRequest, result model.CheckResult) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, c.checkKey(req), data, c.ttl).Err()
}

// Invalidate deletes all cached check results for a (user, resource_type, resource_id) triple.
// Uses SCAN + DEL — never KEYS.
func (c *Cache) Invalidate(ctx context.Context, userID, resourceType, resourceID string) error {
	pattern := fmt.Sprintf("authz:check:%s:%s:%s:*", userID, resourceType, resourceID)
	var cursor uint64
	for {
		keys, nextCursor, err := c.client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := c.client.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return nil
}

// FlushAll deletes all authz check cache entries using SCAN + DEL.
// Never calls FLUSHDB — safe on shared Redis instances.
func (c *Cache) FlushAll(ctx context.Context) error {
	pattern := "authz:check:*"
	var cursor uint64
	for {
		keys, nextCursor, err := c.client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := c.client.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return nil
}

// Ping checks Redis connectivity.
func (c *Cache) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}

// Close closes the Redis client.
func (c *Cache) Close() error {
	return c.client.Close()
}
