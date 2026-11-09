package cache

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"authz-service/internal/model"

	"github.com/redis/go-redis/v9"
)

type Cache struct {
	client *redis.Client
}

func NewCache(addr string) *Cache {
	client := redis.NewClient(&redis.Options{
		Addr: addr,
	})
	return &Cache{client: client}
}

func (c *Cache) Client() *redis.Client {
	return c.client
}

// checkKeyPrefix starts every check-cache key.
const checkKeyPrefix = "authz:check:"

// keyEscaper escapes one key component, so ":" only ever separates
// components and no two requests share a key.
var keyEscaper = strings.NewReplacer(`\`, `\\`, `:`, `\:`)

// globEscaper escapes Redis glob metacharacters in an already key-escaped
// component, so a SCAN pattern matches it literally.
var globEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`)

// checkKey is authz:check:{user}:{resource_type}:{resource_id}:{action},
// each component escaped (\ → \\, : → \:).
func (c *Cache) checkKey(req model.CheckRequest) string {
	return checkKeyPrefix + strings.Join([]string{
		keyEscaper.Replace(req.UserID.String()),
		keyEscaper.Replace(req.ResourceType),
		keyEscaper.Replace(req.ResourceID),
		keyEscaper.Replace(req.Action),
	}, ":")
}

// invalidatePattern is the SCAN pattern for every action cached on one
// (user, resource_type, resource_id): its components match literally.
func invalidatePattern(userID, resourceType, resourceID string) string {
	parts := []string{userID, resourceType, resourceID}
	for i, p := range parts {
		parts[i] = globEscaper.Replace(keyEscaper.Replace(p))
	}
	return checkKeyPrefix + strings.Join(parts, ":") + ":*"
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

// Set caches a check result for ttl. The caller picks the ttl, so a result
// never outlives the grant that decided it.
func (c *Cache) Set(ctx context.Context, req model.CheckRequest, result model.CheckResult, ttl time.Duration) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, c.checkKey(req), data, ttl).Err()
}

// Invalidate deletes all cached check results for a (user, resource_type, resource_id) triple.
// Uses SCAN + DEL — never KEYS.
func (c *Cache) Invalidate(ctx context.Context, userID, resourceType, resourceID string) error {
	pattern := invalidatePattern(userID, resourceType, resourceID)
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
	pattern := checkKeyPrefix + "*"
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
