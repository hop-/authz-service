//go:build integration

package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"authz-service/internal/cache"
	"authz-service/internal/db"
	"authz-service/internal/kafkatopics"
	"authz-service/internal/model"
	"authz-service/internal/producer"
	"authz-service/internal/testinfra"
	"authz-service/internal/worker"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
)

// One Postgres, Redis and Kafka for the package. Each test uses its own
// topics, consumer group and user ids.
var (
	pool      *pgxpool.Pool
	redisAddr string
	brokers   []string
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	var stops []func() error
	exit := func(code int) {
		for i := len(stops) - 1; i >= 0; i-- {
			if err := stops[i](); err != nil {
				fmt.Fprintln(os.Stderr, "stop container:", err)
			}
		}
		os.Exit(code)
	}

	p, stop, err := testinfra.StartPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres:", err)
		exit(1)
	}
	pool, stops = p, append(stops, stop)

	addr, stop, err := testinfra.StartRedis(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start redis:", err)
		exit(1)
	}
	redisAddr, stops = addr, append(stops, stop)

	b, stop, err := testinfra.StartKafka(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start kafka:", err)
		exit(1)
	}
	brokers, stops = b, append(stops, stop)

	exit(m.Run())
}

const waitFor = 90 * time.Second

var actor = uuid.MustParse("00000000-0000-0000-0000-0000000000bb")

// uniq returns a name unique to this test run.
func uniq(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

// workerProc is one running worker: handler, consumer and producer, as
// cmd/worker wires them. Workers keep no registry (CL-1).
type workerProc struct {
	cache *cache.Cache
	done  chan error
}

func startWorker(t *testing.T, ctx context.Context, topic, group, id string) *workerProc {
	t.Helper()
	q := db.NewQueries(pool, testinfra.AcquireTimeout)
	c := cache.NewCache(redisAddr)
	prod := producer.NewProducer(strings.Join(brokers, ","), topic)
	h := worker.NewHandler(q, c, prod, id)
	consumer := worker.NewConsumer(brokers, topic, group, h)

	runCtx, cancel := context.WithCancel(ctx)
	w := &workerProc{cache: c, done: make(chan error, 1)}
	go func() { w.done <- consumer.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-w.done:
		case <-time.After(30 * time.Second):
			t.Errorf("worker %s did not stop", id)
		}
		if err := consumer.Close(); err != nil {
			t.Logf("close consumer %s: %v", id, err)
		}
		if err := prod.Close(); err != nil {
			t.Logf("close producer %s: %v", id, err)
		}
		if err := c.Close(); err != nil {
			t.Logf("close cache %s: %v", id, err)
		}
	})
	return w
}

func newPublisher(t *testing.T, topic string) *producer.Producer {
	t.Helper()
	p := producer.NewProducer(strings.Join(brokers, ","), topic)
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Logf("close publisher: %v", err)
		}
	})
	return p
}

func publish(t *testing.T, p *producer.Producer, key string, evt map[string]interface{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.Publish(ctx, key, evt); err != nil {
		t.Fatalf("publish %v: %v", evt["event"], err)
	}
}

func roleAssigned(user uuid.UUID, role, resourceID string) map[string]interface{} {
	e := producer.NewEventEnvelope("role.assigned")
	e["user_id"] = user.String()
	e["role_name"] = role
	e["resource_type"] = "document"
	e["resource_id"] = resourceID
	e["scope"] = map[string]string{}
	e["granted_by"] = actor.String()
	return e
}

// eventually polls cond until it holds or waitFor passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func userRoleCount(t *testing.T, user uuid.UUID, resourceID string) int {
	t.Helper()
	return countRows(t, `SELECT count(*) FROM user_roles WHERE user_id = $1 AND resource_id = $2`, user, resourceID)
}

func cacheKey(user uuid.UUID, resourceID, action string) string {
	return fmt.Sprintf("authz:check:%s:document:%s:%s", user, resourceID, action)
}

func setKey(t *testing.T, c *cache.Cache, key string) {
	t.Helper()
	if err := c.Client().Set(context.Background(), key, `{"allowed":false,"source":"default_deny"}`, time.Hour).Err(); err != nil {
		t.Fatalf("SET %s: %v", key, err)
	}
}

func keyExists(t *testing.T, c *cache.Cache, key string) bool {
	t.Helper()
	n, err := c.Client().Exists(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("EXISTS %s: %v", key, err)
	}
	return n == 1
}

// dlqMessages reads every message currently on the DLQ topic.
func dlqMessages(t *testing.T, topic string) []model.DLQMessage {
	t.Helper()
	client := &kafka.Client{Addr: kafka.TCP(brokers...)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	meta, err := client.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{topic}})
	if err != nil {
		t.Fatalf("metadata %s: %v", topic, err)
	}
	var out []model.DLQMessage
	for _, tp := range meta.Topics {
		if tp.Error != nil {
			return nil // topic absent
		}
		for _, p := range tp.Partitions {
			out = append(out, readPartition(t, ctx, client, topic, p.ID)...)
		}
	}
	return out
}

func readPartition(t *testing.T, ctx context.Context, client *kafka.Client, topic string, partition int) []model.DLQMessage {
	t.Helper()
	offsets, err := client.ListOffsets(ctx, &kafka.ListOffsetsRequest{
		Topics: map[string][]kafka.OffsetRequest{topic: {kafka.FirstOffsetOf(partition), kafka.LastOffsetOf(partition)}},
	})
	if err != nil {
		t.Fatalf("list offsets: %v", err)
	}
	po := offsets.Topics[topic][0]
	if po.LastOffset <= po.FirstOffset {
		return nil
	}
	r := kafka.NewReader(kafka.ReaderConfig{Brokers: brokers, Topic: topic, Partition: partition, MinBytes: 1, MaxBytes: 10e6})
	defer func() { _ = r.Close() }() // read-only helper; a close error changes nothing
	if err := r.SetOffset(po.FirstOffset); err != nil {
		t.Fatalf("set offset: %v", err)
	}
	var out []model.DLQMessage
	for off := po.FirstOffset; off < po.LastOffset; off++ {
		msg, err := r.ReadMessage(ctx)
		if err != nil {
			t.Fatalf("read DLQ: %v", err)
		}
		var m model.DLQMessage
		if err := json.Unmarshal(msg.Value, &m); err != nil {
			t.Fatalf("decode DLQ message: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// committedOffsets returns the group's committed offset per partition (-1: none).
func committedOffsets(t *testing.T, group, topic string) map[int]int64 {
	t.Helper()
	client := &kafka.Client{Addr: kafka.TCP(brokers...)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := client.OffsetFetch(ctx, &kafka.OffsetFetchRequest{
		GroupID: group,
		Topics:  map[string][]int{topic: {0, 1, 2}},
	})
	if err != nil {
		t.Fatalf("offset fetch: %v", err)
	}
	out := map[int]int64{}
	for _, p := range res.Topics[topic] {
		out[p.Partition] = p.CommittedOffset
	}
	return out
}

func anyCommitted(offsets map[int]int64) bool {
	for _, o := range offsets {
		if o > 0 {
			return true
		}
	}
	return false
}

// --- Scenarios ---

func TestWorker_RoleAssignedWritesRowAndInvalidatesScope(t *testing.T) {
	topic := uniq("e2e.assign")
	testinfra.CreateTopics(t, brokers, topic, topic+".dlq")
	ctx := context.Background()
	w := startWorker(t, ctx, topic, uniq("group"), "w1")
	pub := newPublisher(t, topic)
	user := uuid.New()
	scoped, otherResource := cacheKey(user, "d1", "read"), cacheKey(user, "d2", "read")
	setKey(t, w.cache, scoped)
	setKey(t, w.cache, otherResource)

	publish(t, pub, user.String(), roleAssigned(user, "editor", "d1"))

	eventually(t, "user_roles row", func() bool { return userRoleCount(t, user, "d1") == 1 })
	eventually(t, "scoped cache key removed", func() bool { return !keyExists(t, w.cache, scoped) })
	if !keyExists(t, w.cache, otherResource) {
		t.Error("cache key for another resource was invalidated")
	}
	if msgs := dlqMessages(t, topic+".dlq"); len(msgs) != 0 {
		t.Errorf("unexpected DLQ messages: %+v", msgs)
	}
}

// TestWorker_DuplicateEventRunsFollowUpsOnly redelivers an override.set whose
// payload differs (set_by, reason). Its upsert would overwrite the row if it
// ran again, so an unchanged row proves the dedup skipped the write (008 F-006).
func TestWorker_DuplicateEventRunsFollowUpsOnly(t *testing.T) {
	topic := uniq("e2e.dup")
	testinfra.CreateTopics(t, brokers, topic, topic+".dlq")
	ctx := context.Background()
	w := startWorker(t, ctx, topic, uniq("group"), "w1")
	pub := newPublisher(t, topic)
	user := uuid.New()
	otherActor := uuid.New()
	evt := producer.NewEventEnvelope("override.set")
	evt["user_id"] = user.String()
	evt["action"], evt["resource_type"], evt["resource_id"] = "read", "document", "d1"
	evt["scope"] = map[string]string{}
	evt["effect"], evt["reason"], evt["set_by"] = "DENY", "first", actor.String()
	evt["expires_at"] = nil
	eventID := evt["event_id"].(string)
	key := cacheKey(user, "d1", "read")

	readOverride := func() (setBy uuid.UUID, reason string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT set_by, reason FROM user_permission_overrides WHERE user_id = $1`, user).
			Scan(&setBy, &reason); err != nil {
			t.Fatalf("read override: %v", err)
		}
		return setBy, reason
	}
	overrideCount := func() int {
		return countRows(t, `SELECT count(*) FROM user_permission_overrides WHERE user_id = $1`, user)
	}

	publish(t, pub, user.String(), evt)
	eventually(t, "first delivery applied", func() bool { return overrideCount() == 1 })
	eventually(t, "cache invalidated", func() bool { return !keyExists(t, w.cache, key) })
	if setBy, reason := readOverride(); setBy != actor || reason != "first" {
		t.Fatalf("first delivery stored set_by %s reason %q", setBy, reason)
	}

	// A stale entry cached after the first delivery; the redelivery must
	// invalidate it again without touching the DB.
	setKey(t, w.cache, key)
	dup := make(map[string]interface{}, len(evt))
	for k, v := range evt {
		dup[k] = v
	}
	dup["set_by"], dup["reason"] = otherActor.String(), "second" // same event_id
	publish(t, pub, user.String(), dup)

	eventually(t, "duplicate's follow-up invalidation", func() bool { return !keyExists(t, w.cache, key) })
	if n := overrideCount(); n != 1 {
		t.Errorf("override rows = %d, want 1", n)
	}
	if n := countRows(t, `SELECT count(*) FROM event_log WHERE event_id = $1`, eventID); n != 1 {
		t.Errorf("event_log rows = %d, want 1", n)
	}
	if setBy, reason := readOverride(); setBy != actor || reason != "first" {
		t.Errorf("duplicate event rewrote the row: set_by %s reason %q, want %s \"first\"", setBy, reason, actor)
	}
}

func createRoleInDB(t *testing.T, name string) int16 {
	t.Helper()
	q := db.NewQueries(pool, testinfra.AcquireTimeout)
	var id int16
	applied, err := q.ApplyEvent(context.Background(), uuid.New(), "role.created", func(w db.Writer) error {
		var err error
		id, err = w.InsertRole(context.Background(), name, "test")
		return err
	})
	if err != nil || !applied {
		t.Fatalf("create role: (%v, %v)", applied, err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO role_permissions (role_id, permission_id)
		SELECT $1, id FROM permissions WHERE action = 'read' AND resource_type = 'document'`, id); err != nil {
		t.Fatalf("grant read: %v", err)
	}
	return id
}

// TestWorker_RoleResolvedFromDB: a role the worker never saw an event for (it
// was created after the worker started) is resolved from the DB in the tx.
func TestWorker_RoleResolvedFromDB(t *testing.T) {
	topic := uniq("e2e.miss")
	testinfra.CreateTopics(t, brokers, topic, topic+".dlq")
	ctx := context.Background()
	startWorker(t, ctx, topic, uniq("group"), "w1")
	role := uniq("auditor")
	roleID := createRoleInDB(t, role)
	pub := newPublisher(t, topic)
	user := uuid.New()

	publish(t, pub, user.String(), roleAssigned(user, role, "d1"))

	eventually(t, "assignment with a DB-resolved role", func() bool { return userRoleCount(t, user, "d1") == 1 })
	if n := countRows(t, `SELECT count(*) FROM user_roles WHERE user_id = $1 AND role_id = $2`, user, roleID); n != 1 {
		t.Errorf("assignment rows with role id %d = %d, want 1", roleID, n)
	}
	if msgs := dlqMessages(t, topic+".dlq"); len(msgs) != 0 {
		t.Errorf("unexpected DLQ messages: %+v", msgs)
	}
}

// TestWorker_TwoWorkersMixedStream covers AC-2: two workers in one group
// process registry events and user events correctly.
func TestWorker_TwoWorkersMixedStream(t *testing.T) {
	topic := uniq("e2e.multi")
	testinfra.CreateTopics(t, brokers, topic, topic+".dlq")
	ctx := context.Background()
	group := uniq("group")
	startWorker(t, ctx, topic, group, "w1")
	startWorker(t, ctx, topic, group, "w2")
	assignment := awaitSplitAssignment(t, group, topic)
	pub := newPublisher(t, topic)

	// Registry events: a new role, through the "registry" partition. Only one
	// worker processes it.
	role := uniq("reviewer")
	created := producer.NewEventEnvelope("role.created")
	created["role_name"] = role
	publish(t, pub, "registry", created)
	q := db.NewQueries(pool, testinfra.AcquireTimeout)
	eventually(t, "role created by a worker", func() bool {
		_, found, err := q.RoleID(ctx, role)
		return err == nil && found
	})
	rid, _, _ := q.RoleID(ctx, role)
	perm := producer.NewEventEnvelope("role_permissions.assigned")
	perm["role_name"], perm["action"], perm["resource_type"] = role, "read", "document"
	publish(t, pub, "registry", perm)
	eventually(t, "role permission granted", func() bool {
		return countRows(t, `SELECT count(*) FROM role_permissions WHERE role_id = $1`, rid) == 1
	})

	// User events spread over all partitions, so both workers get some; the
	// one that didn't process role.created resolves the role from the DB.
	users := make([]uuid.UUID, 24)
	for i := range users {
		users[i] = uuid.New()
		role := role
		if i%3 == 0 {
			role = "viewer"
		}
		publish(t, pub, users[i].String(), roleAssigned(users[i], role, "d1"))
	}
	revoked := users[1]
	revoke := producer.NewEventEnvelope("role.revoked")
	revoke["user_id"], revoke["role_name"], revoke["resource_type"], revoke["resource_id"] = revoked.String(), role, "document", "d1"
	publish(t, pub, revoked.String(), revoke)

	eventually(t, "all user events applied", func() bool {
		for _, u := range users {
			want := 1
			if u == revoked {
				want = 0
			}
			if userRoleCount(t, u, "d1") != want {
				return false
			}
		}
		return true
	})
	if msgs := dlqMessages(t, topic+".dlq"); len(msgs) != 0 {
		t.Fatalf("mixed stream dead-lettered %d events: %+v", len(msgs), msgs[0].Failure)
	}

	// Both workers took part: users landed on partitions of both members.
	owners := map[string]bool{}
	for _, u := range users {
		owners[assignment[partitionOf(u.String())]] = true
	}
	if len(owners) != 2 {
		t.Fatalf("user events reached members %v; the test needs both workers to take part", owners)
	}
}

// partitionOf returns the partition the producer's hash balancer picks for key
// on a topic created by testinfra.CreateTopics.
func partitionOf(key string) int { return partitionOfN(key, testinfra.TopicPartitions) }

func partitionOfN(key string, partitions int) int {
	parts := make([]int, partitions)
	for i := range parts {
		parts[i] = i
	}
	return (&kafka.Hash{}).Balance(kafka.Message{Key: []byte(key)}, parts...)
}

// awaitSplitAssignment waits until the group is Stable with two members that
// both hold partitions of topic, and returns partition -> member id.
func awaitSplitAssignment(t *testing.T, group, topic string) map[int]string {
	t.Helper()
	return awaitAssignment(t, group, topic, 2, testinfra.TopicPartitions)
}

// awaitAssignment waits until the group is Stable with `members` members, each
// holding partitions of topic, and `partitions` partitions assigned in total. It
// returns partition -> member id.
func awaitAssignment(t *testing.T, group, topic string, members, partitions int) map[int]string {
	t.Helper()
	client := &kafka.Client{Addr: kafka.TCP(brokers...)}
	var last string
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		res, err := client.DescribeGroups(ctx, &kafka.DescribeGroupsRequest{GroupIDs: []string{group}})
		cancel()
		if err == nil && len(res.Groups) == 1 && res.Groups[0].Error == nil {
			g := res.Groups[0]
			owner := map[int]string{}
			holders := 0
			for _, m := range g.Members {
				held := false
				for _, tp := range m.MemberAssignments.Topics {
					if tp.Topic != topic {
						continue
					}
					for _, p := range tp.Partitions {
						owner[p] = m.MemberID
						held = true
					}
				}
				if held {
					holders++
				}
			}
			if g.GroupState == "Stable" && holders == members && len(owner) == partitions {
				return owner
			}
			last = fmt.Sprintf("state %s, %d holders, %d partitions assigned", g.GroupState, holders, len(owner))
		} else {
			last = fmt.Sprintf("describe: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("group %s never reached %d members holding %d partitions of %s (%s)", group, members, partitions, topic, last)
	return nil
}

// TestWorker_RoleRecreatedAppliesAgainstNewID covers AC-2 (CL-1): two workers;
// the registry-partition worker removes a role and re-creates the same name;
// grants and revokes for it, handled by the other worker, apply against the
// new id. With a cached id they would hit the old, deleted one.
func TestWorker_RoleRecreatedAppliesAgainstNewID(t *testing.T) {
	topic := uniq("e2e.recreate")
	testinfra.CreateTopics(t, brokers, topic, topic+".dlq")
	ctx := context.Background()
	group := uniq("group")
	startWorker(t, ctx, topic, group, "w1")
	startWorker(t, ctx, topic, group, "w2")
	owner := awaitSplitAssignment(t, group, topic)
	registryOwner := owner[partitionOf("registry")]
	pub := newPublisher(t, topic)
	q := db.NewQueries(pool, testinfra.AcquireTimeout)
	role := uniq("editor")

	createRole := func() int16 {
		t.Helper()
		created := producer.NewEventEnvelope("role.created")
		created["role_name"] = role
		publish(t, pub, "registry", created)
		var id int16
		eventually(t, "role created", func() bool {
			got, found, err := q.RoleID(ctx, role)
			id = got
			return err == nil && found
		})
		perm := producer.NewEventEnvelope("role_permissions.assigned")
		perm["role_name"], perm["action"], perm["resource_type"] = role, "read", "document"
		publish(t, pub, "registry", perm)
		eventually(t, "role permission granted", func() bool {
			return countRows(t, `SELECT count(*) FROM role_permissions WHERE role_id = $1`, id) == 1
		})
		return id
	}

	// Users whose events land on partitions the other worker owns.
	var users []uuid.UUID
	for len(users) < 3 {
		u := uuid.New()
		if owner[partitionOf(u.String())] != registryOwner {
			users = append(users, u)
		}
	}
	grant := func(u uuid.UUID, resourceID string) { publish(t, pub, u.String(), roleAssigned(u, role, resourceID)) }
	revoke := func(u uuid.UUID, resourceID string) {
		e := producer.NewEventEnvelope("role.revoked")
		e["user_id"], e["role_name"], e["resource_type"], e["resource_id"] = u.String(), role, "document", resourceID
		publish(t, pub, u.String(), e)
	}

	// 1. The role exists; the other worker grants with the first id.
	oldID := createRole()
	grant(users[0], "d1")
	eventually(t, "grant with the first id", func() bool { return userRoleCount(t, users[0], "d1") == 1 })

	// 2. The registry worker removes the role (cascade drops the grant) and
	// re-creates the same name with a new id.
	removed := producer.NewEventEnvelope("role.removed")
	removed["role_name"] = role
	publish(t, pub, "registry", removed)
	eventually(t, "role removed", func() bool {
		_, found, err := q.RoleID(ctx, role)
		return err == nil && !found
	})
	// 008 F-007: removing the role cascaded the first grant away.
	if n := userRoleCount(t, users[0], "d1"); n != 0 {
		t.Fatalf("grant with the removed role survived role.removed: %d rows", n)
	}
	if n := countRows(t, `SELECT count(*) FROM user_roles WHERE role_id = $1`, oldID); n != 0 {
		t.Fatalf("rows reference the removed role id %d right after removal: %d", oldID, n)
	}
	newID := createRole()
	if newID == oldID {
		t.Fatalf("re-created role kept id %d; the test needs a new id", newID)
	}

	// 3. Grants and a revoke through the other worker use the new id.
	grant(users[1], "d1")
	grant(users[2], "d1")
	grant(users[0], "d2")
	revoke(users[2], "d1")
	eventually(t, "grants and revoke after re-create", func() bool {
		return userRoleCount(t, users[1], "d1") == 1 && userRoleCount(t, users[0], "d2") == 1 &&
			userRoleCount(t, users[2], "d1") == 0
	})
	// users[2]'s revoke removed the row its grant wrote: with a stale id the
	// grant would have been dead-lettered and the revoke would delete nothing.
	// No row may reference the removed id.
	if n := countRows(t, `SELECT count(*) FROM user_roles WHERE role_id = $1`, oldID); n != 0 {
		t.Errorf("rows still reference the removed role id %d: %d", oldID, n)
	}
	for _, check := range []struct {
		user uuid.UUID
		res  string
	}{{users[1], "d1"}, {users[0], "d2"}} {
		if n := countRows(t, `SELECT count(*) FROM user_roles WHERE user_id = $1 AND resource_id = $2 AND role_id = $3`,
			check.user, check.res, newID); n != 1 {
			t.Errorf("grant for %s/%s with the new role id %d: %d rows, want 1", check.user, check.res, newID, n)
		}
	}
	if msgs := dlqMessages(t, topic+".dlq"); len(msgs) != 0 {
		t.Fatalf("events dead-lettered after re-create (a stale id would do this): %+v", msgs[0].Failure)
	}
}

// TestWorker_TopicCreatedAfterStart covers F-012 the way cmd/worker starts:
// the process waits for the main and DLQ topics (kafkatopics.Wait), and once
// infra creates them it starts the consumer and processes events, with no
// restart.
func TestWorker_TopicCreatedAfterStart(t *testing.T) {
	topic := uniq("e2e.late")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waited := make(chan error, 1)
	go func() { waited <- kafkatopics.Wait(ctx, brokers, topic, topic+".dlq") }()

	select {
	case err := <-waited:
		t.Fatalf("Wait returned %v before the topics existed", err)
	case <-time.After(3 * time.Second):
	}

	testinfra.CreateTopics(t, brokers, topic)
	select {
	case err := <-waited:
		t.Fatalf("Wait returned %v with the DLQ topic still missing", err)
	case <-time.After(2 * time.Second):
	}

	testinfra.CreateTopics(t, brokers, topic+".dlq")
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("Wait = %v, want nil once both topics exist", err)
		}
	case <-time.After(waitFor):
		t.Fatal("Wait did not return after the topics were created")
	}

	startWorker(t, ctx, topic, uniq("group"), "w1")
	pub := newPublisher(t, topic)
	user := uuid.New()
	publish(t, pub, user.String(), roleAssigned(user, "viewer", "d1"))

	eventually(t, "event on a topic created after start", func() bool { return userRoleCount(t, user, "d1") == 1 })
}

// TestWorker_PartitionsAddedAfterJoin covers WatchPartitionChanges: partitions
// added to the topic after the group joined are consumed without a restart.
func TestWorker_PartitionsAddedAfterJoin(t *testing.T) {
	topic := uniq("e2e.grow")
	testinfra.CreateTopics(t, brokers, topic, topic+".dlq")
	ctx := context.Background()
	group := uniq("group")
	startWorker(t, ctx, topic, group, "w1")
	awaitAssignment(t, group, topic, 1, testinfra.TopicPartitions)

	const grown = 2 * testinfra.TopicPartitions
	client := &kafka.Client{Addr: kafka.TCP(brokers...)}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := client.CreatePartitions(cctx, &kafka.CreatePartitionsRequest{
		Topics: []kafka.TopicPartitionsConfig{{Name: topic, Count: grown}},
	})
	if err != nil {
		t.Fatalf("create partitions: %v", err)
	}
	for name, err := range res.Errors {
		if err != nil {
			t.Fatalf("create partitions for %s: %v", name, err)
		}
	}
	awaitAssignment(t, group, topic, 1, grown)

	// Users whose events land on the new partitions only.
	pub := newPublisher(t, topic)
	var users []uuid.UUID
	for len(users) < 3 {
		u := uuid.New()
		if partitionOfN(u.String(), grown) >= testinfra.TopicPartitions {
			users = append(users, u)
			publish(t, pub, u.String(), roleAssigned(u, "viewer", "d1"))
		}
	}
	eventually(t, "events on partitions added after the join", func() bool {
		for _, u := range users {
			if userRoleCount(t, u, "d1") != 1 {
				return false
			}
		}
		return true
	})
}

// TestWorker_DLQUnavailableBlocksCommit covers F-001: while the DLQ topic is
// missing, the dead-lettered event is not committed and the publish keeps
// retrying; once the topic exists, it is published and committed.
func TestWorker_DLQUnavailableBlocksCommit(t *testing.T) {
	topic := uniq("e2e.dlq")
	testinfra.CreateTopics(t, brokers, topic) // no DLQ topic yet
	ctx := context.Background()
	group := uniq("group")
	startWorker(t, ctx, topic, group, "w1")

	raw := &kafka.Writer{Addr: kafka.TCP(brokers...), Topic: topic, Balancer: &kafka.Hash{}}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Logf("close writer: %v", err)
		}
	})
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := raw.WriteMessages(wctx, kafka.Message{Key: []byte("bad"), Value: []byte(`{not json`)}); err != nil {
		t.Fatalf("write bad message: %v", err)
	}

	// Give the worker time to fetch the message and fail several DLQ publishes.
	time.Sleep(8 * time.Second)
	if offsets := committedOffsets(t, group, topic); anyCommitted(offsets) {
		t.Fatalf("offset committed while the DLQ topic is missing: %v", offsets)
	}

	testinfra.CreateTopics(t, brokers, topic+".dlq")

	var msgs []model.DLQMessage
	eventually(t, "DLQ message published after the topic appears", func() bool {
		msgs = dlqMessages(t, topic+".dlq")
		return len(msgs) > 0
	})
	if len(msgs) != 1 || msgs[0].Failure.Reason != model.DLQReasonPermanent || string(msgs[0].OriginalPayload) != `{not json` {
		t.Fatalf("DLQ messages = %+v, want the bad message once as permanent", msgs)
	}
	eventually(t, "offset committed after the DLQ publish", func() bool {
		return anyCommitted(committedOffsets(t, group, topic))
	})
}
