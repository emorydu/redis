// Copyright 2023 Emory.Du <orangeduxiaocheng@gmail.com>. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package storage

import (
	"context"
	"crypto/tls"
	"fmt"
	"github.com/emorydu/errors"
	"github.com/emorydu/log"
	redis "github.com/go-redis/redis/v7"
	uuid "github.com/satori/go.uuid"
	"github.com/spf13/viper"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Config defines options for redis cluster.
type Config struct {
	Host                  string
	Port                  int
	Addrs                 []string
	MasterName            string
	Username              string
	Password              string
	Database              int
	MaxIdle               int
	MaxActive             int
	Timeout               int
	EnableCluster         bool
	UseSSL                bool
	SSLInsecureSkipVerify bool
}

// ErrRedisIsDown is returned when we can't communicate with redis.
var ErrRedisIsDown = errors.New("storage: Redis is either down or ws not configured")

var (
	singlePool      atomic.Value
	singleCachePool atomic.Value
	redisUp         atomic.Value
)

var disableRedis atomic.Value

// DisableRedis very handy when testing it allows to dynamically enable/disable talking with redisW.
func DisableRedis(ok bool) {
	if ok {
		redisUp.Store(false)
		disableRedis.Store(true)

		return
	}
	redisUp.Store(true)
	disableRedis.Store(false)
}

func shouldConnect() bool {
	if v := disableRedis.Load(); v != nil {
		return !v.(bool)
	}

	return true
}

// Connected returns true if we are connected to redis.
func Connected() bool {
	if v := redisUp.Load(); v != nil {
		return v.(bool)
	}

	return false
}

func singleton(cache bool) redis.UniversalClient {
	if cache {
		v := singleCachePool.Load()
		if v != nil {
			return v.(redis.UniversalClient)
		}

		return nil
	}
	if v := singlePool.Load(); v != nil {
		return v.(redis.UniversalClient)
	}

	return nil
}

// nolint: unparam
func connectSingleton(cache bool, config *Config) bool {
	if singleton(cache) == nil {
		log.Debug("Connecting to redis cluster")
		if cache {
			singleCachePool.Store(NewRedisClusterPool(cache, config))

			return true
		}
		singlePool.Store(NewRedisClusterPool(cache, config))

		return true
	}

	return true
}

// RedisCluster is a storage manager that uses the redis database.
type RedisCluster struct {
	KeyPrefix string
	HashKeys  bool
	IsCache   bool
}

func clusterConnectionIsOpen(cluster RedisCluster) bool {
	c := singleton(cluster.IsCache)
	testKey := "redis-test-" + uuid.Must(uuid.NewV4()).String()
	if err := c.Set(testKey, "test", time.Second).Err(); err != nil {
		log.Warnf("Error trying to set test key: %s", err.Error())

		return false
	}
	if _, err := c.Get(testKey).Result(); err != nil {
		log.Warnf("Error trying to get test key: %s", err.Error())

		return false
	}

	return true
}

// ConnectToRedis starts a goroutine that periodically tries to connect to redis.
func ConnectToRedis(ctx context.Context, config *Config) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	c := []RedisCluster{
		{}, {IsCache: true},
	}
	var ok bool
	for _, v := range c {
		if !connectSingleton(v.IsCache, config) {
			break
		}

		if !clusterConnectionIsOpen(v) {
			redisUp.Store(false)
			break
		}
		ok = true
	}
	redisUp.Store(ok)
again:
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !shouldConnect() {
				continue
			}
			for _, v := range c {
				if !connectSingleton(v.IsCache, config) {
					redisUp.Store(false)

					goto again
				}

				if !clusterConnectionIsOpen(v) {
					redisUp.Store(false)

					goto again
				}
			}
			redisUp.Store(true)
		}
	}
}

func NewRedisClusterPool(isCache bool, config *Config) redis.UniversalClient {
	// redisSingletonMu is locked and we know the singleton is nil
	log.Debug("Creating new Redis connection pool")

	poolSize := 500
	if config.MaxActive > 0 {
		poolSize = config.MaxActive
	}

	timeout := 5 * time.Second

	if config.Timeout > 0 {
		timeout = time.Duration(config.Timeout) * time.Second
	}

	var tlsConfig *tls.Config

	if config.UseSSL {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: config.SSLInsecureSkipVerify,
		}
	}

	var client redis.UniversalClient
	opts := &RedisOpts{
		Addrs:        getRedisAddrs(config),
		DB:           config.Database,
		Password:     config.Password,
		DialTimeout:  timeout,
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
		PoolSize:     poolSize,
		IdleTimeout:  240 * timeout,
		TLSConfig:    tlsConfig,
		MasterName:   config.MasterName,
	}

	if opts.MasterName != "" {
		log.Info("--> [REDIS] Creating sentinel-backed failover client.")
		client = redis.NewFailoverClient(opts.failover())
	} else if config.EnableCluster {
		log.Info("--> [REDIS] Creating cluster client")
		client = redis.NewClusterClient(opts.cluster())
	} else {
		log.Info("--> [REDIS] Creating single-node client")
		client = redis.NewClient(opts.simple())
	}

	return client
}

type RedisOpts redis.UniversalOptions

func getRedisAddrs(config *Config) (addrs []string) {
	if len(config.Addrs) != 0 {
		addrs = config.Addrs
	}

	if len(addrs) == 0 && config.Port != 0 {
		addr := config.Host + ":" + strconv.Itoa(config.Port)
		addrs = append(addrs, addr)
	}

	return addrs
}

func (o *RedisOpts) cluster() *redis.ClusterOptions {
	if len(o.Addrs) == 0 {
		o.Addrs = []string{"127.0.0.1:6379"}
	}

	return &redis.ClusterOptions{
		Addrs:     o.Addrs,
		OnConnect: o.OnConnect,

		Password: o.Password,

		MaxRedirects:   o.MaxRedirects,
		ReadOnly:       o.ReadOnly,
		RouteByLatency: o.RouteByLatency,
		RouteRandomly:  o.RouteRandomly,

		MaxRetries:      o.MaxRetries,
		MinRetryBackoff: o.MinRetryBackoff,
		MaxRetryBackoff: o.MaxRetryBackoff,

		DialTimeout:        o.DialTimeout,
		ReadTimeout:        o.ReadTimeout,
		WriteTimeout:       o.WriteTimeout,
		PoolSize:           o.PoolSize,
		MinIdleConns:       o.MinIdleConns,
		MaxConnAge:         o.MaxConnAge,
		PoolTimeout:        o.PoolTimeout,
		IdleTimeout:        o.IdleTimeout,
		IdleCheckFrequency: o.IdleCheckFrequency,

		TLSConfig: o.TLSConfig,
	}
}

func (o *RedisOpts) simple() *redis.Options {
	addr := "127.0.0.1:6379"
	if len(o.Addrs) > 0 {
		addr = o.Addrs[0]
	}

	return &redis.Options{
		Addr:      addr,
		OnConnect: o.OnConnect,

		DB:       o.DB,
		Password: o.Password,

		MaxRetries:      o.MaxRetries,
		MinRetryBackoff: o.MinRetryBackoff,
		MaxRetryBackoff: o.MaxRetryBackoff,

		DialTimeout:  o.DialTimeout,
		ReadTimeout:  o.ReadTimeout,
		WriteTimeout: o.WriteTimeout,

		PoolSize:           o.PoolSize,
		MinIdleConns:       o.MinIdleConns,
		MaxConnAge:         o.MaxConnAge,
		PoolTimeout:        o.PoolTimeout,
		IdleTimeout:        o.IdleTimeout,
		IdleCheckFrequency: o.IdleCheckFrequency,

		TLSConfig: o.TLSConfig,
	}
}

func (o *RedisOpts) failover() *redis.FailoverOptions {
	if len(o.Addrs) == 0 {
		o.Addrs = []string{"127.0.0.1:26379"}
	}

	return &redis.FailoverOptions{
		SentinelAddrs: o.Addrs,
		MasterName:    o.MasterName,
		OnConnect:     o.OnConnect,

		DB:       o.DB,
		Password: o.Password,

		MaxRetries:      o.MaxRetries,
		MinRetryBackoff: o.MinRetryBackoff,
		MaxRetryBackoff: o.MaxRetryBackoff,

		DialTimeout:  o.DialTimeout,
		ReadTimeout:  o.ReadTimeout,
		WriteTimeout: o.WriteTimeout,

		PoolSize:           o.PoolSize,
		MinIdleConns:       o.MinIdleConns,
		MaxConnAge:         o.MaxConnAge,
		PoolTimeout:        o.PoolTimeout,
		IdleTimeout:        o.IdleTimeout,
		IdleCheckFrequency: o.IdleCheckFrequency,

		TLSConfig: o.TLSConfig,
	}
}

// Connect will establish a connection this is always true because we are dynamically using redis.
func (r *RedisCluster) Connect() bool {
	return true
}

func (r *RedisCluster) singleton() redis.UniversalClient {
	return singleton(r.IsCache)
}

func (r *RedisCluster) hashKey(in string) string {
	if !r.HashKeys {
		// Not hashing? Return the raw key
		return in
	}

	return HashStr(in)
}

func (r *RedisCluster) fixKey(key string) string {
	return r.KeyPrefix + r.hashKey(key)
}

func (r *RedisCluster) cleanKey(key string) string {
	return strings.Replace(key, r.KeyPrefix, "", 1)
}

func (r *RedisCluster) up() error {
	if !Connected() {
		return ErrRedisIsDown
	}

	return nil
}

// GetKey will retrieve a key from the database.
func (r *RedisCluster) GetKey(key string) (string, error) {
	if err := r.up(); err != nil {
		return "", err
	}

	cluster := r.singleton()

	value, err := cluster.Get(r.fixKey(key)).Result()
	if err != nil {
		log.Debugf("Error trying to get value: %s", err.Error())

		return "", ErrKeyNotFound
	}

	return value, nil
}

func (r *RedisCluster) GetMultiKey(keys []string) ([]string, error) {
	if err := r.up(); err != nil {
		return nil, err
	}
	cluster := r.singleton()
	cpKeys := make([]string, len(keys))
	copy(cpKeys, keys)
	for i, val := range cpKeys {
		cpKeys[i] = r.fixKey(val)
	}

	result := make([]string, 0)

	switch v := cluster.(type) {
	case *redis.ClusterClient:
		{
			commands := make([]*redis.StringCmd, 0)
			pipeline := v.Pipeline()
			for _, key := range cpKeys {
				commands = append(commands, pipeline.Get(key))
			}
			_, err := pipeline.Exec()
			if err != nil && !errors.Is(err, redis.Nil) {
				log.Debugf("Error trying to get value: %s", err.Error())

				return nil, ErrKeyNotFound
			}
			for _, cmd := range commands {
				result = append(result, cmd.Val())
			}
		}
	case *redis.Client:
		{
			values, err := cluster.MGet(cpKeys...).Result()
			if err != nil {
				log.Debugf("Error trying to get value: %s", err.Error())

				return nil, ErrKeyNotFound
			}
			for _, val := range values {
				valString := fmt.Sprint(val)
				if valString == "<nil>" {
					valString = ""
				}
				result = append(result, valString)
			}
		}
	}

	for _, val := range result {
		if val != "" {
			return result, nil
		}
	}

	return nil, ErrKeyNotFound
}

// GetKeyTLS return ttl of the given key.
func (r *RedisCluster) GetKeyTLS(key string) (ttl int64, err error) {
	if err := r.up(); err != nil {
		return 0, err
	}
	duration, err := r.singleton().TTL(r.fixKey(key)).Result()

	return int64(duration.Seconds()), err
}

// GetRawKey return the value of given key.
func (r *RedisCluster) GetRawKey(key string) (string, error) {
	if err := r.up(); err != nil {
		return "", err
	}
	value, err := r.singleton().Get(key).Result()
	if err != nil {
		log.Debugf("Error trying to get value: %s", err.Error())

		return "", ErrKeyNotFound
	}

	return value, err
}

// GetExp return the expiry of the given key.
func (r *RedisCluster) GetExp(key string) (int64, error) {
	log.Debugf("Getting exp for key: %s", r.fixKey(key))
	if err := r.up(); err != nil {
		return 0, err
	}

	value, err := r.singleton().TTL(r.fixKey(key)).Result()
	if err != nil {
		log.Errorf("Error trying to get TTL:", err.Error())

		return 0, ErrKeyNotFound
	}

	return int64(value.Seconds()), nil
}

// SetExp set expiry of the given key.
func (r *RedisCluster) SetExp(key string, timeout time.Duration) error {
	if err := r.up(); err != nil {
		return err
	}
	err := r.singleton().Expire(r.fixKey(key), timeout).Err()
	if err != nil {
		log.Errorf("Could not EXPIRE key: %s", err.Error())
	}

	return err
}

// SetKey will create (or update) a key value in the store.
func (r *RedisCluster) SetKey(key, session string, timeout time.Duration) error {
	log.Debugf("[STORE] SET Raw key is: %s", key)
	log.Debugf("[STORE] Setting key: %s", r.fixKey(key))

	if err := r.up(); err != nil {
		return err
	}

	err := r.singleton().Set(r.fixKey(key), session, timeout).Err()
	if err != nil {
		log.Errorf("Error trying to set value: %s", err.Error())

		return err
	}

	return nil
}

// SetRawKey set the value of the given key.
func (r *RedisCluster) SetRawKey(key, session string, timeout time.Duration) error {
	if err := r.up(); err != nil {
		return err
	}
	err := r.singleton().Set(key, session, timeout).Err()
	if err != nil {
		log.Errorf("Error trying to set value: %s", err.Error())

		return err
	}

	return nil
}

// Decrement will decrement a key in redis.
func (r *RedisCluster) Decrement(key string) {
	key = r.fixKey(key)
	log.Debugf("Decrementing key: %s", key)
	if err := r.up(); err != nil {
		return
	}
	if err := r.singleton().Decr(key).Err(); err != nil {
		log.Errorf("Error trying to decrement value: %s", err.Error())
	}
}

// IncrementWithExpire will increment a key in redis.
func (r *RedisCluster) IncrementWithExpire(key string, expire int64) int64 {
	log.Debugf("Incrementing raw key: %s", key)
	if err := r.up(); err != nil {
		return 0
	}
	// This function uses a raw key, so we shouldn't call fixKey
	fixedKey := key
	val, err := r.singleton().Incr(fixedKey).Result()
	if err != nil {
		log.Errorf("Error trying to increment value: %s", err.Error())
	} else {
		log.Debugf("Incremented key: %s, val is: %d", fixedKey, val)
	}

	if val == 1 && expire > 0 {
		log.Debug("--> Setting Expire")
		r.singleton().Expire(fixedKey, time.Duration(expire)*time.Second)
	}

	return val
}

// GetKeys will return all keys according to the filter (filter is a prefix - e.g. tyk.keys.*).
func (r *RedisCluster) GetKeys(filter string) []string {
	if err := r.up(); err != nil {
		return nil
	}
	client := r.singleton()

	filterHash := ""
	if filter != "" {
		filterHash = r.hashKey(filter)
	}
	searchString := r.KeyPrefix + filterHash + "*"
	log.Debugf("[STORE] Getting list by: %s", searchString)

	fnFetchKeys := func(client *redis.Client) ([]string, error) {
		values := make([]string, 0)

		iter := client.Scan(0, searchString, 0).Iterator()
		for iter.Next() {
			values = append(values, iter.Val())
		}

		if err := iter.Err(); err != nil {
			return nil, err
		}

		return values, nil
	}

	var err error
	var values []string
	sessions := make([]string, 0)

	switch v := client.(type) {
	case *redis.ClusterClient:
		ch := make(chan []string)

		go func() {
			err = v.ForEachMaster(func(client *redis.Client) error {
				values, err = fnFetchKeys(client)
				if err != nil {
					return err
				}

				ch <- values

				return nil
			})
			close(ch)
		}()

		for res := range ch {
			sessions = append(sessions, res...)
		}
	case *redis.Client:
		sessions, err = fnFetchKeys(v)
	}

	if err != nil {
		log.Errorf("Error while fetching keys: %s", err)

		return nil
	}

	for i, v := range sessions {
		sessions[i] = r.cleanKey(v)
	}

	return sessions
}

// GetKeysAndValuesWithFilter will return all keys and their values with a filter.
func (r *RedisCluster) GetKeysAndValuesWithFilter(filter string) map[string]string {
	if err := r.up(); err != nil {
		return nil
	}
	keys := r.GetKeys(filter)
	if keys == nil {
		log.Errorf("Error trying to get filtered client keys.")

		return nil
	}

	for i, v := range keys {
		keys[i] = r.KeyPrefix + v
	}

	client := r.singleton()
	values := make([]string, 0)

	switch v := client.(type) {
	case *redis.ClusterClient:
		{
			commands := make([]*redis.StringCmd, 0)
			pipeline := v.Pipeline()
			for _, key := range keys {
				commands = append(commands, pipeline.Get(key))
			}
			_, err := pipeline.Exec()
			if err != nil && !errors.Is(err, redis.Nil) {
				log.Errorf("Error trying to get client keys: %s", err.Error())

				return nil
			}

			for _, cmd := range commands {
				values = append(values, cmd.Val())
			}
		}
	case *redis.Client:
		{
			result, err := v.MGet(keys...).Result()
			if err != nil {
				log.Errorf("Error trying to get client keys: %s", err.Error())

				return nil
			}

			for _, val := range result {
				valString := fmt.Sprint(val)
				if valString == "<nil>" {
					valString = ""
				}
				values = append(values, valString)
			}
		}
	}

	m := make(map[string]string)
	for i, v := range keys {
		m[r.cleanKey(v)] = values[i]
	}

	return m
}

// GetKeysAndValues will return all keys and their values - not to be used lightly.
func (r *RedisCluster) GetKeysAndValues() map[string]string {
	return r.GetKeysAndValuesWithFilter("")
}

// DeleteKey will remove a key from the database.
func (r *RedisCluster) DeleteKey(key string) bool {
	if err := r.up(); err != nil {
		return false
	}

	log.Debugf("DEL Key was: %s", key)
	log.Debugf("DEL Key became: %s", r.fixKey(key))
	n, err := r.singleton().Del(r.fixKey(key)).Result()
	if err != nil {
		log.Errorf("Error trying to delete key: %s", err.Error())
	}

	return n > 0
}

// DeleteAllKeys will remove all keys from the database.
func (r *RedisCluster) DeleteAllKeys() bool {
	if err := r.up(); err != nil {
		return false
	}
	n, err := r.singleton().FlushAll().Result()
	if err != nil {
		log.Errorf("Error trying to delete keys: %s", err.Error())
	}

	if n == "OK" {
		return true
	}

	return false
}

// DeleteRawKey will remove key from the database without prefixing, assumes user knows what they are doing.
func (r *RedisCluster) DeleteRawKey(key string) bool {
	if err := r.up(); err != nil {
		return false
	}
	n, err := r.singleton().Del(key).Result()
	if err != nil {
		log.Errorf("Error trying to delete key: %s", err.Error())
	}

	return n > 0
}

// DeleteScanMatch will remove a group of keys in bulk.
func (r *RedisCluster) DeleteScanMatch(pattern string) bool {
	if err := r.up(); err != nil {
		return false
	}
	client := r.singleton()
	log.Debugf("Deleting: %s", pattern)

	fnScan := func(client *redis.Client) ([]string, error) {
		values := make([]string, 0)

		iter := client.Scan(0, pattern, 0).Iterator()
		for iter.Next() {
			values = append(values, iter.Val())
		}

		if err := iter.Err(); err != nil {
			return nil, err
		}
		return values, nil
	}

	var err error
	var keys []string
	var values []string

	switch v := client.(type) {
	case *redis.ClusterClient:
		ch := make(chan []string)
		go func() {
			err = v.ForEachMaster(func(client *redis.Client) error {
				values, err = fnScan(client)
				if err != nil {
					return err
				}

				ch <- values

				return nil
			})
			close(ch)
		}()

		for vals := range ch {
			keys = append(keys, vals...)
		}
	case *redis.Client:
		keys, err = fnScan(v)
	}

	if err != nil {
		log.Errorf("SCAN command field with err: %s", err.Error())

		return false
	}

	if len(keys) > 0 {
		for _, name := range keys {
			log.Infof("Deleting: %s", name)
			err := client.Del(name).Err()
			if err != nil {
				log.Errorf("Error trying to delete key: %s - %s", name, err.Error())
			}
		}
		log.Infof("Deleted: %d records", len(keys))
	} else {
		log.Debug("RedisCluster called DEL - Nothing to delete")
	}

	return true
}

// DeleteKeys will remove a group of keys in bulk.
func (r *RedisCluster) DeleteKeys(keys []string) bool {
	if err := r.up(); err != nil {
		return false
	}
	if len(keys) > 0 {
		for i, v := range keys {
			keys[i] = r.fixKey(v)
		}

		log.Debugf("Deleting: %v", keys)
		client := r.singleton()
		switch v := client.(type) {
		case *redis.ClusterClient:
			{
				pipe := v.Pipeline()
				for _, k := range keys {
					pipe.Del(k)
				}

				if _, err := pipe.Exec(); err != nil {
					log.Errorf("Error trying to delete keys: %s", err.Error())
				}
			}
		case *redis.Client:
			{
				_, err := v.Del(keys...).Result()
				if err != nil {
					log.Errorf("Error trying to delete keys: %s", err.Error())
				}
			}
		}
	} else {
		log.Debug("RedisCluster called DEL - Nothing to delete")
	}

	return true
}

// StartPubSubHandler will listen for a signal and run the callback for
// every subscription and message event.
func (r *RedisCluster) StartPubSubHandler(channel string, callback func(interface{})) error {
	if err := r.up(); err != nil {
		return err
	}
	client := r.singleton()
	if client == nil {
		return errors.New("redis connection failed")
	}

	pubsub := client.Subscribe(channel)
	defer pubsub.Close()

	if _, err := pubsub.Receive(); err != nil {
		log.Errorf("Error while receiving pubsub message: %s", err.Error())

		return err
	}

	for msg := range pubsub.Channel() {
		callback(msg)
	}

	return nil
}

// Publish a message to the specify channel.
func (r *RedisCluster) Publish(channel, message string) error {
	if err := r.up(); err != nil {
		return err
	}
	err := r.singleton().Publish(channel, message).Err()
	if err != nil {
		log.Errorf("Error trying to set value: %s", err.Error())

		return err
	}

	return nil
}

// GetAndDeleteSet get and delete a key.
func (r *RedisCluster) GetAndDeleteSet(key string) []interface{} {
	log.Debugf("Getting raw key set: %s", key)
	if err := r.up(); err != nil {
		return nil
	}
	log.Debugf("key is: %s", key)
	fixedKey := r.fixKey(key)
	log.Debugf("Fixed key is: %s", fixedKey)

	client := r.singleton()

	var lrange *redis.StringSliceCmd
	_, err := client.TxPipelined(func(pipe redis.Pipeliner) error {
		lrange = pipe.LRange(fixedKey, 0, -1)
		pipe.Del(fixedKey)

		return nil
	})
	if err != nil {
		log.Errorf("Multi command failed: %s", err.Error())

		return nil
	}

	vals := lrange.Val()
	log.Debugf("Analytics returned: %d", len(vals))
	if len(vals) == 0 {
		return nil
	}

	log.Debugf("Unpacked vals: %d", len(vals))
	result := make([]interface{}, len(vals))
	for i, v := range vals {
		result[i] = v
	}

	return result
}

// AppendToSet append a value to the key set.
func (r *RedisCluster) AppendToSet(key, value string) {
	fixedKey := r.fixKey(key)
	log.Debug("Pushing to raw key list", log.String("key", key))
	log.Debug("Appending to fixed key list", log.String("fixedKey", fixedKey))
	if err := r.up(); err != nil {
		return
	}
	if err := r.singleton().RPush(fixedKey, value).Err(); err != nil {
		log.Errorf("Error trying to append to set keys: %s", err.Error())
	}
}

// Exists check if key exists.
func (r *RedisCluster) Exists(key string) (bool, error) {
	fixedKey := r.fixKey(key)
	log.Debug("Checking if exists", log.String("key", fixedKey))

	exists, err := r.singleton().Exists(fixedKey).Result()
	if err != nil {
		log.Errorf("Error trying to check if key exists: %s", err.Error())

		return false, err
	}
	if exists == 1 {
		return true, nil
	}

	return false, nil
}

// RemoveFromList delete a value from a list identified with the key.
func (r *RedisCluster) RemoveFromList(key, value string) error {
	fixedKey := r.fixKey(key)

	log.Debug("Removing value from list",
		log.String("key", key),
		log.String("fixedKey", fixedKey),
		log.String("value", value))

	if err := r.singleton().LRem(fixedKey, 0, value).Err(); err != nil {
		log.Error("LREM command failed",
			log.String("key", key),
			log.String("fixedKey", fixedKey),
			log.String("value", value),
			log.String("error", err.Error()),
		)

		return err
	}

	return nil
}

// GetListRange gets range of elements of list identified by key.
func (r *RedisCluster) GetListRange(key string, from, to int64) ([]string, error) {
	fixedKey := r.fixKey(key)

	elements, err := r.singleton().LRange(fixedKey, from, to).Result()
	if err != nil {
		log.Error(
			"LRANGE command failed",
			log.String("key", key),
			log.String("fixedKey", fixedKey),
			log.Int64("from", from),
			log.Int64("to", to),
			log.String("error", err.Error()),
		)

		return nil, err
	}

	return elements, nil
}

// AppendToSetPipelined append values to redis pipeline.
func (r *RedisCluster) AppendToSetPipelined(key string, values [][]byte) {
	if len(values) == 0 {
		return
	}

	fixedKey := r.fixKey(key)
	if err := r.up(); err != nil {
		log.Debug(err.Error())

		return
	}

	client := r.singleton()
	pipeline := client.Pipeline()

	for _, val := range values {
		pipeline.RPush(fixedKey, val)
	}

	if _, err := pipeline.Exec(); err != nil {
		log.Errorf("Error trying to append to set keys: %s", err.Error())
	}

	if storageExpTime := int64(viper.GetDuration("analytics.storage-expiration-time")); storageExpTime != int64(-1) {
		// If there is no expiry on the analytics set, we should set it.
		exp, _ := r.GetExp(key)
		if exp == -1 {
			_ = r.SetExp(key, time.Duration(storageExpTime)*time.Second)
		}
	}
}

// GetSet return key set value.
func (r *RedisCluster) GetSet(key string) (map[string]string, error) {
	log.Debugf("Getting from key set: %s", key)
	log.Debugf("Getting from fixed key set: %s", r.fixKey(key))
	if err := r.up(); err != nil {
		return nil, err
	}

	vals, err := r.singleton().SMembers(r.fixKey(key)).Result()
	if err != nil {
		log.Errorf("Error trying to get key set: %s", err.Error())

		return nil, err
	}

	result := make(map[string]string)
	for i, v := range vals {
		result[strconv.Itoa(i)] = v
	}

	return result, nil
}

// AddToSet add value to key set.
func (r *RedisCluster) AddToSet(key, value string) {
	log.Debugf("Pushing to raw key set: %s", key)
	log.Debugf("Pushing to fixed key set: %s", r.fixKey(key))
	if err := r.up(); err != nil {
		return
	}

	if err := r.singleton().SAdd(r.fixKey(key), value).Err(); err != nil {
		log.Errorf("Error trying to append keys: %s", err.Error())
	}
}

// RemoveFromSet remove a value from key set.
func (r *RedisCluster) RemoveFromSet(key, value string) {
	log.Debugf("Removing from raw key set: %s", key)
	log.Debugf("Removing from fixed key set: %s", r.fixKey(key))
	if err := r.up(); err != nil {
		log.Debug(err.Error())

		return
	}

	if err := r.singleton().SRem(r.fixKey(key), value).Err(); err != nil {
		log.Errorf("Error trying to remove keys: %s", err.Error())
	}
}

// IsMemberOfSet return whether the given value belong to key set.
func (r *RedisCluster) IsMemberOfSet(key, value string) bool {
	if err := r.up(); err != nil {
		log.Debug(err.Error())

		return false
	}

	val, err := r.singleton().SIsMember(r.fixKey(key), value).Result()
	if err != nil {
		log.Errorf("Error trying to check set member: %s", err.Error())

		return false
	}

	log.Debugf("DISMEMBER %s %s %v %v", key, value, val, err)

	return val
}

// SetRollingWindow will append to a sorted set in redis and extract a timed window of values.
func (r *RedisCluster) SetRollingWindow(
	key string,
	per int64,
	valueOverride string,
	pipeline bool) (int, []interface{}) {
	log.Debugf("Incrementing raw key: %s", key)
	if err := r.up(); err != nil {
		log.Debug(err.Error())

		return 0, nil
	}

	log.Debugf("key is: %s", key)
	now := time.Now()
	log.Debugf("Now is: %v", now)
	onePeriodAgo := now.Add(time.Duration(-1*per) * time.Second)
	log.Debugf("Then is: %v", onePeriodAgo)

	client := r.singleton()
	var zrange *redis.StringSliceCmd

	pipeFn := func(pipe redis.Pipeliner) error {
		pipe.ZRemRangeByScore(key, "-inf", strconv.Itoa(int(onePeriodAgo.UnixNano())))
		zrange = pipe.ZRange(key, 0, -1)

		element := redis.Z{
			Score: float64(now.UnixNano()),
		}

		if valueOverride != "-1" {
			element.Member = valueOverride
		} else {
			element.Member = strconv.Itoa(int(now.UnixNano()))
		}

		pipe.ZAdd(key, &element)
		pipe.Expire(key, time.Duration(per)*time.Second)

		return nil
	}

	var err error
	if pipeline {
		_, err = client.Pipelined(pipeFn)
	} else {
		_, err = client.TxPipelined(pipeFn)
	}
	if err != nil {
		log.Errorf("Multi command failed: %s", err.Error())

		return 0, nil
	}

	values := zrange.Val()

	// Check actual value
	if values == nil {
		return 0, nil
	}

	vsLen := len(values)
	result := make([]interface{}, vsLen)

	for i, v := range values {
		result[i] = v
	}

	log.Debugf("Returned: %d", vsLen)

	return vsLen, result
}

// GetRollingWindow return rolling window.
func (r *RedisCluster) GetRollingWindow(
	key string,
	per int64,
	pipeline bool) (int, []interface{}) {
	if err := r.up(); err != nil {
		log.Debug(err.Error())

		return 0, nil
	}

	now := time.Now()
	onePeriodAgo := now.Add(time.Duration(-1*per) * time.Second)

	client := r.singleton()
	var zrange *redis.StringSliceCmd

	pipeFn := func(pipe redis.Pipeliner) error {
		pipe.ZRemRangeByScore(key, "-inf", strconv.Itoa(int(onePeriodAgo.UnixNano())))
		zrange = pipe.ZRange(key, 0, -1)

		return nil
	}

	var err error
	if pipeline {
		_, err = client.Pipelined(pipeFn)
	} else {
		_, err = client.TxPipelined(pipeFn)
	}
	if err != nil {
		log.Errorf("Multi command failed: %s", err.Error())

		return 0, nil
	}

	values := zrange.Val()

	// Check actual value
	if values == nil {
		return 0, nil
	}

	vsLen := len(values)
	result := make([]interface{}, vsLen)

	for i, v := range values {
		result[i] = v
	}

	log.Debugf("Returned: %d", vsLen)

	return vsLen, result
}

// GetKeyPrefix returns storage key prefix.
func (r *RedisCluster) GetKeyPrefix() string {
	return r.KeyPrefix
}

// AddToStoredSet adds value with given score to sorted set identified by key.
func (r *RedisCluster) AddToStoredSet(key, value string, score float64) {
	fixedKey := r.fixKey(key)

	log.Debug("Pushing raw key to sorted set", log.String("key", key), log.String("fixedKey", fixedKey))

	if err := r.up(); err != nil {
		log.Debug(err.Error())

		return
	}

	member := redis.Z{Score: score, Member: value}
	if err := r.singleton().ZAdd(fixedKey, &member).Err(); err != nil {
		log.Error(
			"ZADD command failed",
			log.String("key", key),
			log.String("fixedKey", fixedKey),
			log.String("error", err.Error()))
	}
}

// GetSortedSetRange gets range of elements of sorted set identified by keyName.
func (r *RedisCluster) GetSortedSetRange(key, scoreFrom, scoreTo string) ([]string, []float64, error) {
	fixedKey := r.fixKey(key)
	log.Debug(
		"Getting sorted set range",
		log.String("key", key),
		log.String("fixedKey", fixedKey),
		log.String("scoreFrom", scoreFrom),
		log.String("scoreTo", scoreTo))

	args := redis.ZRangeBy{Min: scoreFrom, Max: scoreTo}
	values, err := r.singleton().ZRangeByScoreWithScores(fixedKey, &args).Result()
	if err != nil {
		log.Error(
			"ZRANGEBYSCORE command failed",
			log.String("key", key),
			log.String("fixedKey", fixedKey),
			log.String("scoreFrom", scoreFrom),
			log.String("scoreTo", scoreTo),
			log.String("error", err.Error()))

		return nil, nil, err
	}

	if len(values) == 0 {
		return nil, nil, nil
	}

	elements := make([]string, len(values))
	scores := make([]float64, len(values))

	for i, v := range values {
		elements[i] = fmt.Sprint(v.Member)
		scores[i] = v.Score
	}

	return elements, scores, nil
}

// RemoveSortedSetRange removes range of elements from sorted set identified by key.
func (r *RedisCluster) RemoveSortedSetRange(key, scoreFrom, scoreTo string) error {
	fixedKey := r.fixKey(key)

	log.Debug(
		"Removing sorted set range",
		log.String("key", key),
		log.String("scoreFrom", scoreFrom),
		log.String("scoreTo", scoreTo))

	if err := r.singleton().ZRemRangeByScore(fixedKey, scoreFrom, scoreTo).Err(); err != nil {
		log.Debug(
			"ZREMRANGEBYSCORE command failed",
			log.String("key", key),
			log.String("fixedKey", fixedKey),
			log.String("scoreFrom", scoreFrom),
			log.String("error", err.Error()))

		return err
	}

	return nil
}
