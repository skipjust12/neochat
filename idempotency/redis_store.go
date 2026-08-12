package idempotency

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// reserveTTL bounds how long a Reserve's in-flight marker survives if the
// process handling it crashes before calling Complete or Release --
// without this, a crash would wedge that key behind ErrInFlight forever.
// Not in the Store interface's doc comment literally, but a deliberate
// addition on top of it: a real deployment needs this even if the
// original in-memory version didn't (a crashed process there just loses
// the whole map).
const reserveTTL = 5 * time.Minute

// envelope is the JSON value stored at idem:{userID}:{key} -- it's how
// Reserve tells "still in flight" apart from "completed, here's the
// cached response" using a single GET.
type envelope struct {
	State    string `json:"state"`              // "in_flight" or "done"
	Response string `json:"response,omitempty"` // base64, only set when State == "done"
}

// RedisStore is a Store backed by Redis, following the swap-in procedure
// Store's doc comment specifies: SET NX for Reserve's claim, SET+TTL for
// Complete, DEL for Release.
type RedisStore struct {
	rdb          *redis.Client
	completedTTL time.Duration
}

// NewRedisStore returns a Store backed by rdb. completedTTL bounds how
// long a finished response stays replayable -- the interface's doc
// comment calls this out explicitly as needed for a real deployment
// (unlike InMemoryStore, which keeps completed entries forever).
func NewRedisStore(rdb *redis.Client, completedTTL time.Duration) *RedisStore {
	return &RedisStore{rdb: rdb, completedTTL: completedTTL}
}

func idemKey(userID, key string) string {
	return fmt.Sprintf("idem:%s:%s", userID, key)
}

func (s *RedisStore) Reserve(ctx context.Context, userID, key string) (Record, bool, error) {
	redisKey := idemKey(userID, key)

	inFlight, err := json.Marshal(envelope{State: "in_flight"})
	if err != nil {
		return Record{}, false, err
	}

	ok, err := s.rdb.SetNX(ctx, redisKey, inFlight, reserveTTL).Result()
	if err != nil {
		return Record{}, false, fmt.Errorf("idempotency: redis setnx: %w", err)
	}
	if ok {
		return Record{}, false, nil
	}

	raw, err := s.rdb.Get(ctx, redisKey).Result()
	if errors.Is(err, redis.Nil) {
		// Lost a race against a Release landing between our failed SETNX
		// and this GET -- harmless, treat as never-seen (see doc comment
		// on this package's swap-in procedure for the race note).
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("idempotency: redis get: %w", err)
	}

	var env envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return Record{}, false, fmt.Errorf("idempotency: unmarshal envelope: %w", err)
	}
	if env.State != "done" {
		return Record{}, false, ErrInFlight
	}

	response, err := base64.StdEncoding.DecodeString(env.Response)
	if err != nil {
		return Record{}, false, fmt.Errorf("idempotency: decode response: %w", err)
	}
	return Record{Response: response}, true, nil
}

func (s *RedisStore) Complete(ctx context.Context, userID, key string, record Record) error {
	env := envelope{State: "done", Response: base64.StdEncoding.EncodeToString(record.Response)}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if err := s.rdb.Set(ctx, idemKey(userID, key), data, s.completedTTL).Err(); err != nil {
		return fmt.Errorf("idempotency: redis set: %w", err)
	}
	return nil
}

func (s *RedisStore) Release(ctx context.Context, userID, key string) error {
	if err := s.rdb.Del(ctx, idemKey(userID, key)).Err(); err != nil {
		return fmt.Errorf("idempotency: redis del: %w", err)
	}
	return nil
}
