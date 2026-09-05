package idempotency

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Must exceed server.requestTimeout, including the bounded accounting cleanup.
const reserveTTL = 15 * time.Minute

type envelope struct {
	State    string `json:"state"`
	Owner    string `json:"owner,omitempty"`
	Response string `json:"response,omitempty"`
}

type RedisStore struct {
	rdb          *redis.Client
	completedTTL time.Duration
}

func NewRedisStore(rdb *redis.Client, completedTTL time.Duration) *RedisStore {
	return &RedisStore{rdb: rdb, completedTTL: completedTTL}
}

func idemKey(userID, key string) string {
	// Length-delimited components cannot collide when a user/key contains ':'.
	return fmt.Sprintf("idem:%d:%s:%s", len(userID), userID, key)
}

var reserveScript = redis.NewScript(`
 local value=redis.call('GET',KEYS[1])
 if value then return value end
 redis.call('SET',KEYS[1],ARGV[1],'PX',ARGV[2])
 return ''
`)

var completeScript = redis.NewScript(`
 local value=redis.call('GET',KEYS[1])
 if not value then return 0 end
 local entry=cjson.decode(value)
 if entry.state~='in_flight' or entry.owner~=ARGV[1] then return 0 end
 redis.call('SET',KEYS[1],ARGV[2],'PX',ARGV[3])
 return 1
`)

var releaseScript = redis.NewScript(`
 local value=redis.call('GET',KEYS[1])
 if not value then return 0 end
 local entry=cjson.decode(value)
 if entry.state~='in_flight' or entry.owner~=ARGV[1] then return 0 end
 redis.call('DEL',KEYS[1])
 return 1
`)

func (s *RedisStore) Reserve(ctx context.Context, userID, key string) (Record, bool, error) {
	owner, err := newOwner()
	if err != nil {
		return Record{}, false, err
	}
	data, err := json.Marshal(envelope{State: "in_flight", Owner: owner})
	if err != nil {
		return Record{}, false, err
	}
	raw, err := reserveScript.Run(ctx, s.rdb, []string{idemKey(userID, key)}, string(data), reserveTTL.Milliseconds()).Text()
	if err != nil {
		return Record{}, false, fmt.Errorf("idempotency: reserve: %w", err)
	}
	if raw == "" {
		return Record{Owner: owner}, false, nil
	}
	var entry envelope
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return Record{}, false, err
	}
	if entry.State != "done" {
		return Record{}, false, ErrInFlight
	}
	response, err := base64.StdEncoding.DecodeString(entry.Response)
	return Record{Response: response}, true, err
}

func (s *RedisStore) Complete(ctx context.Context, userID, key string, record Record) error {
	if record.Owner == "" {
		return ErrLeaseLost
	}
	data, err := json.Marshal(envelope{State: "done", Response: base64.StdEncoding.EncodeToString(record.Response)})
	if err != nil {
		return err
	}
	ttl := s.completedTTL
	if ttl <= 0 {
		return fmt.Errorf("idempotency: completed TTL must be positive")
	}
	ok, err := completeScript.Run(ctx, s.rdb, []string{idemKey(userID, key)}, record.Owner, string(data), ttl.Milliseconds()).Int()
	if err != nil {
		return err
	}
	if ok != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *RedisStore) Release(ctx context.Context, userID, key, owner string) error {
	if owner == "" {
		return ErrLeaseLost
	}
	ok, err := releaseScript.Run(ctx, s.rdb, []string{idemKey(userID, key)}, owner).Int()
	if err != nil {
		return err
	}
	if ok != 1 {
		return ErrLeaseLost
	}
	return nil
}
