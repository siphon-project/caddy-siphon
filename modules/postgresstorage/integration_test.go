package postgresstorage

// Integration tests against a real PostgreSQL. They run when POSTGRES_STORAGE_TEST_DSN is set, and
// skip otherwise, unless POSTGRES_STORAGE_TEST_REQUIRED=1 (as in CI), where a missing database fails
// the run instead of letting it pass having tested nothing.
//
// Each test gets its own table prefix, and each "node" its own connection pool (a distinct
// application_name makes a distinct DSN, so the pools are not shared), as two Caddy processes would.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/jackc/pgx/v5"
)

func baseDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("POSTGRES_STORAGE_TEST_DSN")
	if dsn == "" {
		if os.Getenv("POSTGRES_STORAGE_TEST_REQUIRED") == "1" {
			t.Fatal("POSTGRES_STORAGE_TEST_REQUIRED=1 but POSTGRES_STORAGE_TEST_DSN is not set")
		}
		t.Skip("POSTGRES_STORAGE_TEST_DSN is not set")
	}
	return dsn
}

func withParameter(dsn, parameter string) string {
	if strings.Contains(dsn, "://") {
		if strings.Contains(dsn, "?") {
			return dsn + "&" + parameter
		}
		return dsn + "?" + parameter
	}
	return dsn + " " + parameter
}

func randomPrefix(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "t" + hex.EncodeToString(b[:]) + "_"
}

func admin(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), baseDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func dropTables(t *testing.T, prefix string) {
	t.Helper()
	conn := admin(t)
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `DROP TABLE IF EXISTS `+
			pgx.Identifier{prefix + "objects"}.Sanitize()+`, `+pgx.Identifier{prefix + "locks"}.Sanitize())
	})
}

type nodeOptions struct {
	ttl        time.Duration
	autoCreate *bool
}

// node opens a storage instance, as one Caddy node would, with a pool of its own.
func node(t *testing.T, prefix, name string, options nodeOptions) *Storage {
	t.Helper()
	s := &Storage{DSN: "set-by-test", TablePrefix: prefix, LockTTL: caddy.Duration(options.ttl), AutoCreate: options.autoCreate}
	dsn := withParameter(baseDSN(t), "application_name=caddy-storage-test-"+name)
	if err := s.open(context.Background(), dsn); err != nil {
		t.Fatalf("opening node %s: %v", name, err)
	}
	t.Cleanup(func() {
		// Release anything a test left held, then the instance's own reference.
		s.mu.Lock()
		names := make([]string, 0, len(s.held))
		for lockName := range s.held {
			names = append(names, lockName)
		}
		s.mu.Unlock()
		for _, lockName := range names {
			_ = s.Unlock(context.Background(), lockName)
		}
		_ = s.Cleanup()
	})
	return s
}

func twoNodes(t *testing.T, ttl time.Duration) (*Storage, *Storage, string) {
	t.Helper()
	prefix := randomPrefix(t)
	dropTables(t, prefix)
	return node(t, prefix, "a", nodeOptions{ttl: ttl}), node(t, prefix, "b", nodeOptions{ttl: ttl}), prefix
}

// abandon stops a held lock's heartbeat without releasing it, as a holder that crashed or froze
// would. The lock stays recorded, so a later Unlock acts as the late holder's would.
func abandon(t *testing.T, s *Storage, name string) {
	t.Helper()
	s.mu.Lock()
	lock := s.held[name]
	s.mu.Unlock()
	if lock == nil {
		t.Fatalf("lock %s is not held", name)
	}
	lock.cancel()
	<-lock.done
}

func lockOwner(t *testing.T, prefix, name string) string {
	t.Helper()
	var owner string
	err := admin(t).QueryRow(context.Background(),
		`SELECT owner FROM `+pgx.Identifier{prefix + "locks"}.Sanitize()+` WHERE name = $1`, name).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func TestStoreLoadStatExistsDelete(t *testing.T) {
	a, b, _ := twoNodes(t, 0)
	ctx := context.Background()
	key := "certificates/ca/a.example.com/a.example.com.crt"

	if err := a.Store(ctx, key, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := a.Store(ctx, key, []byte("version two")); err != nil {
		t.Fatal(err)
	}
	got, err := b.Load(ctx, key) // the other node sees it
	if err != nil || string(got) != "version two" {
		t.Fatalf("Load = %q, %v", got, err)
	}
	info, err := b.Stat(ctx, key)
	if err != nil || !info.IsTerminal || info.Size != int64(len("version two")) || info.Key != key || info.Modified.IsZero() {
		t.Fatalf("Stat(file) = %+v, %v", info, err)
	}
	folder, err := b.Stat(ctx, "certificates/ca")
	if err != nil || folder.IsTerminal || folder.Key != "certificates/ca" || folder.Modified.IsZero() {
		t.Fatalf("Stat(folder) = %+v, %v", folder, err)
	}
	if !b.Exists(ctx, key) || !b.Exists(ctx, "certificates/ca") || !b.Exists(ctx, "certificates/ca/") {
		t.Fatal("Exists is false for a stored file or its folder")
	}
	if b.Exists(ctx, "certificates/c") {
		t.Fatal("Exists is true for a partial path segment")
	}
	if err := a.Store(ctx, "empty", nil); err != nil {
		t.Fatal(err)
	}
	if v, err := b.Load(ctx, "empty"); err != nil || v == nil || len(v) != 0 {
		t.Fatalf("Load(empty) = %v, %v", v, err)
	}
	binary := []byte{0, 1, 2, 255, 0}
	if err := a.Store(ctx, "binary", binary); err != nil {
		t.Fatal(err)
	}
	if v, _ := b.Load(ctx, "binary"); !bytes.Equal(v, binary) {
		t.Fatalf("binary value came back as %v", v)
	}

	if err := b.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if a.Exists(ctx, key) || a.Exists(ctx, "certificates/ca") {
		t.Fatal("key or its now-empty folder still exists after Delete")
	}
}

func TestMissingKeysReportNotExistAndDeletingThemSucceeds(t *testing.T) {
	a, _, _ := twoNodes(t, 0)
	ctx := context.Background()
	if _, err := a.Load(ctx, "nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load(missing) error = %v, want fs.ErrNotExist", err)
	}
	if _, err := a.Stat(ctx, "nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(missing) error = %v, want fs.ErrNotExist", err)
	}
	if _, err := a.List(ctx, "nope", false); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("List(missing) error = %v, want fs.ErrNotExist", err)
	}
	// certmagic deletes a site's folder after deleting its files (deleteSiteAssets), when the folder
	// no longer exists; that must not fail.
	if err := a.Delete(ctx, "nope"); err != nil {
		t.Errorf("Delete(missing) = %v, want nil", err)
	}
	if exists := a.Exists(ctx, "nope"); exists {
		t.Error("Exists(missing) = true")
	}
	if err := a.Delete(ctx, ""); err == nil {
		t.Error("Delete(\"\") was accepted; it would empty the table")
	}
	root, err := a.List(ctx, "", true)
	if err != nil || len(root) != 0 {
		t.Errorf("List(root) on an empty store = %v, %v", root, err)
	}
}

func storeAll(t *testing.T, s *Storage, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if err := s.Store(context.Background(), key, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListWithLookalikeNames(t *testing.T) {
	a, b, _ := twoNodes(t, 0)
	ctx := context.Background()
	storeAll(t, a,
		"certificates/ca/wildcard_.example.com/wildcard_.example.com.crt",
		"certificates/ca/wildcard_.example.com/wildcard_.example.com.key",
		"certificates/ca/wildcardX.example.com/wildcardX.example.com.crt",
		"certificates/ca/100%.example.com/100%.example.com.crt",
		"certificates/ca/100X.example.com/100X.example.com.crt",
		"certificates/ca/meta.json",
	)

	direct, err := b.List(ctx, "certificates/ca", false)
	want := []string{
		"certificates/ca/100%.example.com",
		"certificates/ca/100X.example.com",
		"certificates/ca/meta.json",
		"certificates/ca/wildcardX.example.com",
		"certificates/ca/wildcard_.example.com",
	}
	if err != nil || !reflect.DeepEqual(direct, want) {
		t.Fatalf("List non-recursive = %v, %v\nwant %v", direct, err, want)
	}

	wildcard, err := b.List(ctx, "certificates/ca/wildcard_.example.com", false)
	want = []string{
		"certificates/ca/wildcard_.example.com/wildcard_.example.com.crt",
		"certificates/ca/wildcard_.example.com/wildcard_.example.com.key",
	}
	if err != nil || !reflect.DeepEqual(wildcard, want) {
		t.Fatalf("List(wildcard_) = %v, %v\nwant %v", wildcard, err, want)
	}

	percent, err := b.List(ctx, "certificates/ca/100%.example.com", true)
	want = []string{"certificates/ca/100%.example.com/100%.example.com.crt"}
	if err != nil || !reflect.DeepEqual(percent, want) {
		t.Fatalf("List(100%%) = %v, %v\nwant %v", percent, err, want)
	}

	all, err := b.List(ctx, "certificates", true)
	if err != nil || len(all) != 6+5 { // six files, the ca folder, and four site folders
		t.Fatalf("List recursive = %d keys %v, %v", len(all), all, err)
	}
}

// Cleanup of an expired wildcard_ certificate must not delete wildcardX's.
func TestDeletingAFolderLeavesLookalikesAlone(t *testing.T) {
	a, b, _ := twoNodes(t, 0)
	ctx := context.Background()
	storeAll(t, a,
		"certificates/ca/wildcard_.example.com/wildcard_.example.com.crt",
		"certificates/ca/wildcard_.example.com/sub/deeper",
		"certificates/ca/wildcardX.example.com/wildcardX.example.com.crt",
		"certificates/ca/wildcard_.example.comX/z",
		"certificates/ca/100%/x",
		"certificates/ca/100X/x",
	)
	if err := b.Delete(ctx, "certificates/ca/wildcard_.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, "certificates/ca/100%"); err != nil {
		t.Fatal(err)
	}
	left, err := a.List(ctx, "certificates/ca", true)
	want := []string{
		"certificates/ca/100X",
		"certificates/ca/100X/x",
		"certificates/ca/wildcardX.example.com",
		"certificates/ca/wildcardX.example.com/wildcardX.example.com.crt",
		"certificates/ca/wildcard_.example.comX",
		"certificates/ca/wildcard_.example.comX/z",
	}
	if err != nil || !reflect.DeepEqual(left, want) {
		t.Fatalf("after deleting the folders: %v, %v\nwant %v", left, err, want)
	}
}

func TestExistsAndStatDoNotMatchLookalikes(t *testing.T) {
	a, _, _ := twoNodes(t, 0)
	ctx := context.Background()
	storeAll(t, a, "certificates/ca/wildcardX.example.com/wildcardX.example.com.crt", "certificates/ca/100X/x")
	for _, folder := range []string{"certificates/ca/wildcard_.example.com", "certificates/ca/100%"} {
		if a.Exists(ctx, folder) {
			t.Errorf("Exists(%q) is true; only a lookalike exists", folder)
		}
		if _, err := a.Stat(ctx, folder); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Stat(%q) error = %v, want fs.ErrNotExist; only a lookalike exists", folder, err)
		}
	}
}

func TestLockBlocksTheOtherNodeUntilUnlock(t *testing.T) {
	a, b, _ := twoNodes(t, 2*time.Second)
	ctx := context.Background()
	if err := a.Lock(ctx, "issue_example.com"); err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() { acquired <- b.Lock(ctx, "issue_example.com") }()
	select {
	case err := <-acquired:
		t.Fatalf("node b acquired a held lock (err %v)", err)
	case <-time.After(time.Second):
	}
	if err := a.Unlock(ctx, "issue_example.com"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("node b did not acquire the lock after node a released it")
	}
	if ok, err := a.TryLock(ctx, "issue_example.com"); ok || err != nil {
		t.Fatalf("TryLock on a held lock = %v, %v", ok, err)
	}
}

func TestLockGivesUpWhenTheContextEnds(t *testing.T) {
	a, b, _ := twoNodes(t, 2*time.Second)
	if err := a.Lock(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if err := b.Lock(ctx, "x"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock with an ending context = %v, want DeadlineExceeded", err)
	}
}

// With the heartbeat a live holder keeps its lock well past the TTL.
func TestHeartbeatKeepsALiveHoldersLockPastItsTTL(t *testing.T) {
	a, b, _ := twoNodes(t, time.Second)
	ctx := context.Background()
	if err := a.Lock(ctx, "long_issuance"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second) // three TTLs
	if ok, err := b.TryLock(ctx, "long_issuance"); ok || err != nil {
		t.Fatalf("node b took a live holder's lock after 3 TTLs: %v, %v", ok, err)
	}
}

func TestAnExpiredLockIsTakenOver(t *testing.T) {
	a, _, prefix := twoNodes(t, time.Second)
	ctx := context.Background()
	_, err := admin(t).Exec(ctx, `INSERT INTO `+pgx.Identifier{prefix + "locks"}.Sanitize()+
		` (name, owner, expires_at) VALUES ('stale', 'ghost', now() - interval '1 second')`)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := a.TryLock(ctx, "stale")
	if !ok || err != nil {
		t.Fatalf("TryLock on an expired lock = %v, %v", ok, err)
	}
	if owner := lockOwner(t, prefix, "stale"); owner == "ghost" || owner == "" {
		t.Fatalf("owner after takeover = %q", owner)
	}
}

func TestACrashedHoldersLockExpiresAfterItsTTL(t *testing.T) {
	a, b, _ := twoNodes(t, time.Second)
	ctx := context.Background()
	if err := a.Lock(ctx, "crash"); err != nil {
		t.Fatal(err)
	}
	abandon(t, a, "crash")
	if ok, _ := b.TryLock(ctx, "crash"); ok {
		t.Fatal("node b took the lock before its TTL ran out")
	}
	start := time.Now()
	waitContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := b.Lock(waitContext, "crash"); err != nil {
		t.Fatalf("node b never took over a crashed holder's lock: %v", err)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("takeover took %s for a 1s TTL", waited)
	}
}

func TestUnlockByANonOwnerDoesNothing(t *testing.T) {
	a, b, prefix := twoNodes(t, time.Second)
	ctx := context.Background()
	if err := a.Lock(ctx, "owned"); err != nil {
		t.Fatal(err)
	}
	ownerA := lockOwner(t, prefix, "owned")
	if err := b.Unlock(ctx, "owned"); err == nil {
		t.Error("node b unlocked a lock it never held without an error")
	}
	if owner := lockOwner(t, prefix, "owned"); owner != ownerA {
		t.Fatalf("lock owner changed from %q to %q after a non-owner's Unlock", ownerA, owner)
	}
}

// A holder that froze past its TTL comes back and unlocks: the node that took over keeps the lock.
func TestALateHoldersUnlockLeavesTheNewHoldersLock(t *testing.T) {
	a, b, prefix := twoNodes(t, time.Second)
	ctx := context.Background()
	if err := a.Lock(ctx, "zombie"); err != nil {
		t.Fatal(err)
	}
	abandon(t, a, "zombie")
	waitContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := b.Lock(waitContext, "zombie"); err != nil {
		t.Fatal(err)
	}
	ownerB := lockOwner(t, prefix, "zombie")
	if err := a.Unlock(ctx, "zombie"); err != nil {
		t.Fatalf("the late holder's Unlock errored: %v", err)
	}
	if owner := lockOwner(t, prefix, "zombie"); owner != ownerB {
		t.Fatalf("the late holder's Unlock released the new holder's lock (owner now %q, was %q)", owner, ownerB)
	}
	if err := a.RenewLockLease(ctx, "zombie", time.Minute); err == nil {
		t.Fatal("RenewLockLease succeeded for a lock that was taken over")
	}
}

func TestRenewLockLeaseWhileHeld(t *testing.T) {
	a, _, _ := twoNodes(t, time.Second)
	ctx := context.Background()
	if err := a.RenewLockLease(ctx, "never", time.Minute); err == nil {
		t.Fatal("RenewLockLease succeeded for a lock never acquired")
	}
	if err := a.Lock(ctx, "lease"); err != nil {
		t.Fatal(err)
	}
	if err := a.RenewLockLease(ctx, "lease", time.Hour); err != nil {
		t.Fatalf("RenewLockLease while held = %v", err)
	}
}

// N goroutines across both nodes contend for one lock: never two holders at once, and every one of
// them gets its turn.
func TestConcurrentLockersHoldOneAtATime(t *testing.T) {
	a, b, _ := twoNodes(t, 2*time.Second)
	const workers, rounds = 8, 3
	var holders, maximum, acquisitions atomic.Int32
	var wait sync.WaitGroup
	errs := make(chan error, workers*rounds)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for worker := range workers {
		s := a
		if worker%2 == 1 {
			s = b
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range rounds {
				if err := s.Lock(ctx, "contended"); err != nil {
					errs <- err
					return
				}
				now := holders.Add(1)
				for {
					seen := maximum.Load()
					if now <= seen || maximum.CompareAndSwap(seen, now) {
						break
					}
				}
				acquisitions.Add(1)
				time.Sleep(30 * time.Millisecond)
				holders.Add(-1)
				if err := s.Unlock(ctx, "contended"); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if maximum.Load() != 1 {
		t.Fatalf("at most %d holders at once, want exactly 1", maximum.Load())
	}
	if acquisitions.Load() != workers*rounds {
		t.Fatalf("%d acquisitions, want %d", acquisitions.Load(), workers*rounds)
	}
}

// On a reload Caddy cleans up the old storage while an issuance through it may still hold a lock
// and have a certificate to store. Cleanup must leave both working.
func TestCleanupDuringAHeldLockKeepsThePoolAndTheLock(t *testing.T) {
	prefix := randomPrefix(t)
	dropTables(t, prefix)
	dsn := withParameter(baseDSN(t), "application_name=caddy-storage-test-reload")
	old := &Storage{DSN: "set-by-test", TablePrefix: prefix, LockTTL: caddy.Duration(time.Second)}
	if err := old.open(context.Background(), dsn); err != nil {
		t.Fatal(err)
	}
	other := node(t, prefix, "other", nodeOptions{ttl: time.Second})
	ctx := context.Background()

	if err := old.Lock(ctx, "issuance"); err != nil {
		t.Fatal(err)
	}
	if refs := poolReferences(dsn); refs != 2 {
		t.Fatalf("pool references with an instance and a held lock = %d, want 2", refs)
	}
	if err := old.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if refs := poolReferences(dsn); refs != 1 {
		t.Fatalf("pool references after Cleanup with a lock held = %d, want 1", refs)
	}
	time.Sleep(2500 * time.Millisecond) // more than two TTLs: the heartbeat must still run
	if ok, _ := other.TryLock(ctx, "issuance"); ok {
		t.Fatal("the lock expired after Cleanup: its heartbeat stopped")
	}
	if err := old.Store(ctx, "certificates/ca/x/x.crt", []byte("issued")); err != nil {
		t.Fatalf("storing through the cleaned-up instance: %v", err)
	}
	if err := old.Unlock(ctx, "issuance"); err != nil {
		t.Fatal(err)
	}
	if refs := poolReferences(dsn); refs != 0 {
		t.Fatalf("pool references after the last Unlock = %d, want 0 (pool closed)", refs)
	}
	if ok, err := other.TryLock(ctx, "issuance"); !ok || err != nil {
		t.Fatalf("lock not free after Unlock: %v, %v", ok, err)
	}
	if v, err := other.Load(ctx, "certificates/ca/x/x.crt"); err != nil || string(v) != "issued" {
		t.Fatalf("certificate stored during the reload = %q, %v", v, err)
	}
	if err := old.Cleanup(); err != nil { // a second Cleanup is harmless
		t.Fatal(err)
	}
}

func TestTwoInstancesOnOneDSNShareOnePool(t *testing.T) {
	prefix := randomPrefix(t)
	dropTables(t, prefix)
	dsn := withParameter(baseDSN(t), "application_name=caddy-storage-test-shared")
	first := &Storage{DSN: "x", TablePrefix: prefix}
	second := &Storage{DSN: "x", TablePrefix: prefix}
	if err := first.open(context.Background(), dsn); err != nil {
		t.Fatal(err)
	}
	if err := second.open(context.Background(), dsn); err != nil {
		t.Fatal(err)
	}
	if first.shared != second.shared || poolReferences(dsn) != 2 {
		t.Fatalf("instances on one DSN do not share one pool (refs %d)", poolReferences(dsn))
	}
	_ = first.Cleanup()
	if err := second.Store(context.Background(), "k", []byte("v")); err != nil {
		t.Fatalf("the old instance's Cleanup broke the new one: %v", err)
	}
	_ = second.Cleanup()
	if poolReferences(dsn) != 0 {
		t.Fatal("pool still open after both instances were cleaned up")
	}
}

func TestAutoCreateFalseNeedsTheTablesAndUsesThem(t *testing.T) {
	prefix := randomPrefix(t)
	dropTables(t, prefix)
	off := false
	s := &Storage{DSN: "x", TablePrefix: prefix, AutoCreate: &off}
	if err := s.open(context.Background(), baseDSN(t)); err == nil {
		_ = s.Cleanup()
		t.Fatal("auto_create false provisioned without the tables")
	}
	node(t, prefix, "creator", nodeOptions{}) // creates them
	withTables := node(t, prefix, "no-create", nodeOptions{autoCreate: &off})
	if err := withTables.Store(context.Background(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
}

func TestProvisioningTwiceAtOnceCreatesTheTablesOnce(t *testing.T) {
	prefix := randomPrefix(t)
	dropTables(t, prefix)
	var wait sync.WaitGroup
	errs := make(chan error, 6)
	for i := range 6 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			s := &Storage{DSN: "x", TablePrefix: prefix}
			dsn := withParameter(baseDSN(t), "application_name=caddy-storage-test-race-"+string(rune('a'+i)))
			if err := s.open(context.Background(), dsn); err != nil {
				errs <- err
				return
			}
			_ = s.Cleanup()
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent provisioning failed: %v", err)
	}
}

func TestSearchPathInTheDSNPutsTheTablesInTheirOwnSchema(t *testing.T) {
	conn := admin(t)
	schema := strings.TrimSuffix(randomPrefix(t), "_")
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	})
	s := &Storage{DSN: "x"}
	if err := s.open(ctx, withParameter(baseDSN(t), "search_path="+schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Cleanup() })
	var tables int
	err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = $1 AND table_name IN ('caddy_objects', 'caddy_locks')`, schema).Scan(&tables)
	if err != nil || tables != 2 {
		t.Fatalf("tables in schema %s = %d, %v, want 2", schema, tables, err)
	}
}

// Provision resolves {env.X} at run time, which is what keeps the secret out of autosave.json.
func TestProvisionResolvesAnEnvironmentPlaceholder(t *testing.T) {
	prefix := randomPrefix(t)
	dropTables(t, prefix)
	t.Setenv("CADDY_STORAGE_TEST_PLACEHOLDER_DSN", baseDSN(t))
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	s := &Storage{DSN: "{env.CADDY_STORAGE_TEST_PLACEHOLDER_DSN}", TablePrefix: prefix}
	if err := s.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Cleanup() }()
	if err := s.Store(context.Background(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
}
