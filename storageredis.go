package storageredis

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/bsm/redislock"
	"github.com/caddyserver/certmagic"
	"github.com/go-redis/redis/v8"
)

const (
	// LockDuration is lock time duration
	LockDuration = 10 * time.Second

	// LockFreshnessInterval is how often to update a lock's TTL. Locks with a TTL
	// more than this duration in the past (plus a grace period for latency) can be
	// considered stale.
	LockFreshnessInterval = 3 * time.Second

	// LockPollInterval is how frequently to check the existence of a lock
	LockPollInterval = 1 * time.Second

	// Maximum size for the stack trace when recovering from panics.
	stackTraceBufferSize = 1024 * 128

	// ScanCount is how many scan command might return
	ScanCount int64 = 100

	// Default Values

	// DefaultAESKey needs to be 32 bytes long
	DefaultAESKey = ""

	// DefaultKeyPrefix defines the default prefix in KV store
	DefaultKeyPrefix = "caddytls"

	// DefaultValuePrefix sets a prefix to KV values to check validation
	DefaultValuePrefix = "caddy-storage-redis"

	// DefaultRedisHost define the Redis instance host
	DefaultRedisHost = "127.0.0.1"

	// DefaultRedisPort define the Redis instance port
	DefaultRedisPort = "6379"

	// DefaultRedisDB define the Redis DB number
	DefaultRedisDB = 0

	// DefaultRedisPassword define the Redis instance Username, if any
	DefaultRedisUsername = ""

	// DefaultRedisPassword define the Redis instance password, if any
	DefaultRedisPassword = ""

	// DefaultRedisTimeout define the Redis wait time in (s)
	DefaultRedisTimeout = 5

	// DefaultRedisTLS define the Redis TLS connection
	DefaultRedisTLS = false

	// DefaultRedisTLSInsecure define the Redis TLS connection
	DefaultRedisTLSInsecure = true

	// Environment Name

	// EnvNameRedisHost defines the env variable name to override Redis host
	EnvNameRedisHost = "CADDY_CLUSTERING_REDIS_HOST"

	// EnvNameRedisPort defines the env variable name to override Redis port
	EnvNameRedisPort = "CADDY_CLUSTERING_REDIS_PORT"

	// EnvNameRedisDB defines the env variable name to override Redis db number
	EnvNameRedisDB = "CADDY_CLUSTERING_REDIS_DB"

	// EnvNameRedisUsername defines the env variable name to override Redis username
	EnvNameRedisUsername = "CADDY_CLUSTERING_REDIS_USERNAME"

	// EnvNameRedisPassword defines the env variable name to override Redis password
	EnvNameRedisPassword = "CADDY_CLUSTERING_REDIS_PASSWORD"

	// EnvNameRedisTimeout defines the env variable name to override Redis wait timeout for dial, read, write
	EnvNameRedisTimeout = "CADDY_CLUSTERING_REDIS_TIMEOUT"

	// EnvNameAESKey defines the env variable name to override AES key
	EnvNameAESKey = "CADDY_CLUSTERING_REDIS_AESKEY"

	// EnvNameKeyPrefix defines the env variable name to override KV key prefix
	EnvNameKeyPrefix = "CADDY_CLUSTERING_REDIS_KEYPREFIX"

	// EnvNameValuePrefix defines the env variable name to override KV value prefix
	EnvNameValuePrefix = "CADDY_CLUSTERING_REDIS_VALUEPREFIX"

	// EnvNameTLSEnabled defines the env variable name to whether enable Redis TLS Connection or not
	EnvNameTLSEnabled = "CADDY_CLUSTERING_REDIS_TLS"

	// EnvNameTLSInsecure defines the env variable name to whether verify Redis TLS Connection or not
	EnvNameTLSInsecure = "CADDY_CLUSTERING_REDIS_TLS_INSECURE"
)

// RedisStorage contain Redis client, and plugin option
type RedisStorage struct {
	Client       *redis.Client
	ClientLocker *redislock.Client
	Logger       *zap.SugaredLogger
	ctx          context.Context

	Address     string `json:"address"`
	Host        string `json:"host"`
	Port        string `json:"port"`
	DB          int    `json:"db"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	Timeout     int    `json:"timeout"`
	KeyPrefix   string `json:"key_prefix"`
	ValuePrefix string `json:"value_prefix"`
	AesKey      string `json:"aes_key"`
	TlsEnabled  bool   `json:"tls_enabled"`
	TlsInsecure bool   `json:"tls_insecure"`

	locks *sync.Map
}

// StorageData describe the data that is stored in KV storage
type StorageData struct {
	Value    []byte    `json:"value"`
	Modified time.Time `json:"modified"`
}

// CertMagicStorage converts s to a certmagic.Storage instance.
func (rd *RedisStorage) CertMagicStorage() (certmagic.Storage, error) {
	return rd, nil
}

// helper function to prefix key
func (rd *RedisStorage) prefixKey(key string) string {
	return path.Join(rd.KeyPrefix, key)
}

// GetRedisStorage build RedisStorage with it's client
func (rd *RedisStorage) BuildRedisClient() error {
	rd.ctx = context.Background()
	redisClient := redis.NewClient(&redis.Options{
		Addr:         rd.Address,
		Username:     rd.Username,
		Password:     rd.Password,
		DB:           rd.DB,
		DialTimeout:  time.Second * time.Duration(rd.Timeout),
		ReadTimeout:  time.Second * time.Duration(rd.Timeout),
		WriteTimeout: time.Second * time.Duration(rd.Timeout),
	})

	if rd.TlsEnabled {
		redisClient.Options().TLSConfig = &tls.Config{
			InsecureSkipVerify: rd.TlsInsecure,
		}
	}

	_, err := redisClient.Ping(rd.ctx).Result()
	if err != nil {
		return err
	}

	rd.Client = redisClient
	rd.ClientLocker = redislock.New(rd.Client)
	rd.locks = &sync.Map{}
	return nil
}

// Store values at key
func (rd RedisStorage) Store(ctx context.Context, key string, value []byte) error {
	data := &StorageData{
		Value:    value,
		Modified: time.Now(),
	}

	encryptedValue, err := rd.EncryptStorageData(data)
	if err != nil {
		return fmt.Errorf("unable to encode data for %v: %v", key, err)
	}

	if err := rd.Client.Set(rd.ctx, rd.prefixKey(key), encryptedValue, 0).Err(); err != nil {
		return fmt.Errorf("unable to store data for %v: %v", key, err)
	}

	return nil
}

// Load retrieves the value at key.
func (rd RedisStorage) Load(ctx context.Context, key string) ([]byte, error) {
	data, err := rd.getDataDecrypted(key)

	if err != nil {
		return nil, err
	}

	return data.Value, nil
}

// Delete deletes key.
func (rd RedisStorage) Delete(ctx context.Context, key string) error {
	_, err := rd.getData(key)

	if err != nil {
		return err
	}

	if err := rd.Client.Del(rd.ctx, rd.prefixKey(key)).Err(); err != nil {
		return fmt.Errorf("unable to delete data for key %s: %v", key, err)
	}

	return nil
}

// Exists returns true if the key exists
func (rd RedisStorage) Exists(ctx context.Context, key string) bool {
	_, err := rd.getData(key)
	if err == nil {
		return true
	}
	return false
}

// List returns all keys that match prefix.
func (rd RedisStorage) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	var keysFound []string
	var tempKeys []string
	var firstPointer uint64 = 0
	var pointer uint64 = 0
	var search string

	// assuming we want to list all keys
	if prefix == "*" {
		search = rd.prefixKey(prefix)
	} else if len(strings.TrimSpace(prefix)) == 0 {
		search = rd.prefixKey("*")
	} else {
		search = rd.prefixKey(prefix) + "*"
	}

	// first SCAN command
	keys, pointer, err := rd.Client.Scan(rd.ctx, pointer, search, ScanCount).Result()
	if err != nil {
		return keysFound, err
	}
	// store it temporarily
	tempKeys = append(tempKeys, keys...)
	// because SCAN command doesn't always return all possible, keep searching until pointer is equal to the firstPointer
	for pointer != firstPointer {
		keys, nextPointer, _ := rd.Client.Scan(rd.ctx, pointer, search, ScanCount).Result()
		tempKeys = append(tempKeys, keys...)
		pointer = nextPointer
	}

	if prefix == "*" || len(strings.TrimSpace(prefix)) == 0 {
		search = rd.KeyPrefix
	} else {
		search = rd.prefixKey(prefix)
	}

	// remove default prefix from keys
	for _, key := range tempKeys {
		if strings.HasPrefix(key, search) {
			key = strings.TrimPrefix(key, rd.KeyPrefix+"/")
			keysFound = append(keysFound, key)
		}
	}

	// if recursive wanted, or wildcard/empty prefix, just return all keys prefix is empty
	if recursive || prefix == "*" || len(strings.TrimSpace(prefix)) == 0 {
		return keysFound, nil
	}

	// for non-recursive split path and look for unique keys just under given prefix
	keysMap := make(map[string]bool)
	for _, key := range keysFound {
		dir := strings.Split(strings.TrimPrefix(key, prefix+"/"), "/")
		keysMap[dir[0]] = true
	}

	keysFound = make([]string, 0)
	for key := range keysMap {
		keysFound = append(keysFound, path.Join(prefix, key))
	}

	return keysFound, nil
}

// Stat returns information about key.
func (rd RedisStorage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	data, err := rd.getDataDecrypted(key)

	if err != nil {
		return certmagic.KeyInfo{}, err
	}

	return certmagic.KeyInfo{
		Key:        key,
		Modified:   data.Modified,
		Size:       int64(len(data.Value)),
		IsTerminal: false,
	}, nil
}

// getData return data from redis by key as it is
func (rd RedisStorage) getData(key string) ([]byte, error) {
	data, err := rd.Client.Get(rd.ctx, rd.prefixKey(key)).Bytes()

	if err != nil {
		return nil, fmt.Errorf("unable to obtain data for %s: %v", key, err)
	} else if data == nil {
		return nil, fs.ErrNotExist
	}

	return data, nil
}

// getDataDecrypted return StorageData by key
func (rd RedisStorage) getDataDecrypted(key string) (*StorageData, error) {
	data, err := rd.getData(key)

	if err != nil {
		return nil, err
	}

	decryptedData, err := rd.DecryptStorageData(data)

	if err != nil {
		return nil, fmt.Errorf("unable to decrypt data for %s: %v", key, err)
	}

	return decryptedData, nil
}

// Lock is to lock value
func (rd *RedisStorage) Lock(ctx context.Context, key string) error {
	for {
		_, err := rd.obtainLock(key)
		if err == nil {
			// got the lock, yay
			return nil
		}
		if err != redislock.ErrNotObtained {
			// unexpected error
			return fmt.Errorf("creating redis lock: %v", err)
		}

		// lock exists and is not stale;
		// just wait a moment and try again,
		// or return if context cancelled
		select {
		case <-time.After(LockPollInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

func (rd *RedisStorage) obtainLock(key string) (*redislock.Lock, error) {
	lockName := rd.prefixKey(key) + ".lock"

	if lockI, exists := rd.locks.Load(key); exists {
		// check if the lock is stale and cleanup if needed
		if lock, ok := lockI.(*redislock.Lock); ok {
			if ttl, err := lock.TTL(rd.ctx); err != nil {
				return nil, err
			} else if ttl == 0 {
				// lock is dead, clean it up from locks data
				_ = lock.Release(rd.ctx)
				rd.locks.Delete(key)
			}
		}
		// lock already exists, unable to obtain
		return nil, redislock.ErrNotObtained
	} else {
		// obtain new lock
		lock, err := rd.ClientLocker.Obtain(rd.ctx, lockName, LockDuration, &redislock.Options{})
		if err != nil {
			return nil, err
		}

		// save it
		rd.locks.Store(key, lock)

		// keep the lock fresh as long as we hold it
		go rd.keepRedisLockFresh(key)

		return lock, nil
	}
}

// keepRedisLockFresh continuously updates the lock TTL. It stops when
// the lock disappears from rd.locks. Since it pools every
// LockFreshnessInterval, this function might not terminate until up to
// LockFreshnessInterval after the lock is released.
func (rd *RedisStorage) keepRedisLockFresh(key string) {
	defer func() {
		if err := recover(); err != nil {
			buf := make([]byte, stackTraceBufferSize)
			buf = buf[:runtime.Stack(buf, false)]
			rd.Logger.Errorf("panic: active locking: %v\n%s", err, buf)
		}
	}()

	for {
		time.Sleep(LockFreshnessInterval)
		done, err := rd.updateRedisLockFreshness(key)
		if err != nil {
			rd.Logger.Errorf("[ERROR] Keeping redis lock fresh: %v - terminating lock maintenance (lock: %s)", err, key)
			return
		}
		if done {
			return
		}
	}
}

func (rd *RedisStorage) updateRedisLockFreshness(key string) (bool, error) {
	l, exists := rd.locks.Load(key)
	if !exists {
		// lock released
		return true, nil
	}

	lock, ok := l.(*redislock.Lock)
	if !ok {
		return true, fmt.Errorf("uable to cast to redislock")
	}

	// refresh the lock's TTL every LockFreshnessInterval
	err := lock.Refresh(rd.ctx, LockDuration, nil)
	if err != nil {
		rd.Logger.Errorf("[ERROR] Keeping redis lock fresh: %v - terminating lock maintenance (lock: %s)", err, key)
		return true, err
	}

	return false, nil
}

// Unlock is to unlock value
func (rd *RedisStorage) Unlock(ctx context.Context, key string) error {
	if lockI, exists := rd.locks.Load(key); exists {
		if lock, ok := lockI.(*redislock.Lock); ok {
			err := lock.Release(rd.ctx)
			rd.locks.Delete(key)
			if err != nil {
				return fmt.Errorf("we don't have this lock anymore, %v", err)
			}
		}
	}
	return nil
}

func (rd *RedisStorage) GetAESKeyByte() []byte {
	return []byte(rd.AesKey)
}

func (rd RedisStorage) String() string {
	redacted := `REDACTED`
	if rd.Password != "" {
		rd.Password = redacted
	}
	if rd.AesKey != "" {
		rd.AesKey = redacted
	}
	strVal, _ := json.Marshal(rd)
	return string(strVal)
}
