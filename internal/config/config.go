package config

type Config struct {
	// DB config
	DatabaseURL           string `goconfig:"db.url"`
	DBMaxConns            int    `goconfig:"db.conn.max"`
	DBMinConns            int    `goconfig:"db.conn.min"`
	DBMaxConnIdleSecs     int    `goconfig:"db.conn.idleSecs"`
	DBMaxConnLifetimeSecs int    `goconfig:"db.conn.lifetimeSecs"`
	DBAcquireTimeoutSecs  int    `goconfig:"db.conn.acquireTimeoutSecs"`

	// Redis config
	RedisAddr       string `goconfig:"redis.addr"`
	CacheTTLSeconds int    `goconfig:"redis.cacheTtlSeconds"`

	// Kafka config
	KafkaBrokers string `goconfig:"kafka.brokers"`
	KafkaTopic   string `goconfig:"kafka.topic"`
	KafkaGroupID string `goconfig:"kafka.groupId"`

	// Application config
	HTTPPort    int `goconfig:"application.port"`
	MetricsPort int `goconfig:"application.metricsPort"`

	// Admin mode config
	AdminMode  bool   `goconfig:"application.admin.enable"`
	AdminToken string `goconfig:"application.admin.token,optional"`
}
