//go:build integration

// Integration tests for issue #69: the locking from #47 is built on REAL
// API-server behaviour — resourceVersion compare-and-swap (409 Conflict on a
// stale write), AlreadyExists on Create, and label-selector LISTs — and the
// fake clientset enforces none of it (see newArbitratingFakeClientset in
// vfs_concurrent_test.go, the hand-written reactors whose real counterpart
// these tests exercise).
//
// Everything here runs against a real kube-apiserver + etcd started by
// controller-runtime's envtest, behind this build tag so the default
// `go test ./...` stays exactly as it is: no cluster, no binaries.
//
// Run with:
//
//	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25
//	KUBEBUILDER_ASSETS="$(setup-envtest use 1.37.0 -p path)" \
//		go test -race -count=1 -tags integration ./pkg/vfs
//
// The tests live in package vfs so they can reach the same internals the
// fake-clientset tests do: the unexported clock injection (for the lease
// test), the holder ids (for asserting exactly who holds what on the
// cluster), and the busy-retry/assert helpers of vfs_concurrent_test.go,
// which are reused verbatim so the ONLY new variable is the real API server.
package vfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/psanford/sqlite3vfs"
)

// envtestBinariesVersion is the envtest binary set these tests are written
// and documented against (the README and the CI workflow pin the same
// version, which matches the module's k8s.io/* v0.37.x).
const envtestBinariesVersion = "1.37.0"

var (
	// testRestConfig is the envtest rest.Config, with the conflict counter
	// and raised rate limits already applied. Every VFS instance builds its
	// own clientset from a copy of it, the way two separate processes would.
	testRestConfig *rest.Config

	// testKc is an observer clientset used for raw assertions on the stored
	// objects (the "as the API server stores it" checks).
	testKc *kubernetes.Clientset

	// apiConflicts counts HTTP 409 responses seen across every clientset
	// built from testRestConfig: the real API server's compare-and-swap
	// rejections, which the fake clientset can only imitate with reactors.
	apiConflicts atomic.Int64
)

// conflictCountingTransport counts 409s — the API server rejecting a stale
// resourceVersion, which is the exact behaviour issue #47's locking stands
// on and the fake clientset does not have.
type conflictCountingTransport struct {
	rt http.RoundTripper
}

func (c *conflictCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(req)
	if err == nil && resp != nil && resp.StatusCode == http.StatusConflict {
		apiConflicts.Add(1)
	}
	return resp, err
}

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintf(os.Stderr, "issue #69 integration tests: KUBEBUILDER_ASSETS is not set.\n"+
			"Install the envtest binaries and run the tests with:\n"+
			"  go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25\n"+
			"  KUBEBUILDER_ASSETS=\"$(setup-envtest use %s -p path)\" go test -tags integration ./pkg/vfs\n",
			envtestBinariesVersion)
		os.Exit(1)
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "issue #69 integration tests: starting envtest (kube-apiserver + etcd) failed: %v\n"+
			"Is KUBEBUILDER_ASSETS pointing at a %s binary set?\n", err, envtestBinariesVersion)
		os.Exit(1)
	}
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return &conflictCountingTransport{rt: rt}
	}
	// The concurrent tests issue many small calls from several clientsets;
	// the client-go default of QPS 5 would throttle them into timeouts that
	// have nothing to do with what is under test.
	cfg.QPS = 100
	cfg.Burst = 200
	testRestConfig = cfg

	testKc, err = kubernetes.NewForConfig(rest.CopyConfig(testRestConfig))
	if err != nil {
		fmt.Fprintf(os.Stderr, "issue #69 integration tests: build clientset: %v\n", err)
		os.Exit(1)
	}

	// envtest starts apiserver + etcd only: no controller-manager, so the
	// default namespace does not exist. Create the namespace the shared
	// test helpers address (testNamespace), once, for the whole run.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, err = testKc.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: testNamespace},
	}, metav1.CreateOptions{})
	cancel()
	if err != nil && !kerrors.IsAlreadyExists(err) {
		fmt.Fprintf(os.Stderr, "issue #69 integration tests: create namespace %q: %v\n", testNamespace, err)
		os.Exit(1)
	}

	code := m.Run()
	if serr := env.Stop(); serr != nil {
		fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", serr)
	}
	os.Exit(code)
}

// newIntegrationVFS returns a VFS wired to its own clientset — its own
// HTTP transport and rate limiter — over the shared envtest apiserver: what
// a second process looks like, without sharing client-side state that could
// mask a real race.
func newIntegrationVFS(t *testing.T, opts ...Option) *vfs {
	t.Helper()
	kc, err := kubernetes.NewForConfig(rest.CopyConfig(testRestConfig))
	if err != nil {
		t.Fatalf("build clientset: %v", err)
	}
	return NewVFS(t.Context(), kc, testNamespace, slog.New(slog.DiscardHandler), 5, opts...)
}

// getLockConfigMap fetches the lockfile ConfigMap straight from the API
// server, for assertions on the stored bytes.
func getLockConfigMap(t *testing.T, f *file) *corev1.ConfigMap {
	t.Helper()
	cm, err := testKc.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), f.LockFileName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get lock ConfigMap %q: %v", f.LockFileName(), err)
	}
	return cm
}

// storedLockState parses the lockfile ConfigMap as the VFS does.
func storedLockState(t *testing.T, f *file) lockView {
	t.Helper()
	st, err := parseLockState(getLockConfigMap(t, f))
	if err != nil {
		t.Fatalf("parse stored lock state: %v", err)
	}
	return st
}

// b32Name is the base32 encoding the VFS uses for file names (the custom
// lowercase alphabet with 'x' padding, as NewFile builds it).
func b32Name(name string) string {
	return base32.NewEncoding("abcdefghijklmnopqrstuv0123456789").WithPadding('x').EncodeToString([]byte(name))
}

// wantRelevantFileLabel mirrors, independently of the VFS, the
// "relevant-file" label rule of issue #351: the base32 name while it fits
// the 63-character label-value cap, else "long-" + the unpadded lower-case
// base32 of SHA-256(name). (The fake-clientset unit tests pin the VFS to
// exactly these spellings with literal strings, so a drift fails there.)
func wantRelevantFileLabel(name string) string {
	encoded := b32Name(name)
	if len(encoded) <= 63 {
		return encoded
	}
	sum := sha256.Sum256([]byte(name))
	return "long-" + base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding).EncodeToString(sum[:])
}

// TestIntegrationRealAPIServerRejectsStaleResourceVersion pins the
// foundation of issue #47's arbitration on the real server: an Update that
// carries a resourceVersion another writer has already superseded is
// rejected with 409 Conflict — the exact behaviour the fake clientset's
// hand-written reactors imitate.
func TestIntegrationRealAPIServerRejectsStaleResourceVersion(t *testing.T) {
	v := newIntegrationVFS(t)
	f := NewFile(newSQLFileName(), v)
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("first holder Lock(SHARED): %v", err)
	}

	cms := testKc.CoreV1().ConfigMaps(testNamespace)
	cm, err := cms.Get(t.Context(), f.LockFileName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get lock ConfigMap: %v", err)
	}

	// A second writer CASes the same lockfile: the stored resourceVersion
	// moves past the one the first Get observed.
	f2 := NewFile(f.RawName, newIntegrationVFS(t))
	if err := f2.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("second holder Lock(SHARED): %v", err)
	}

	// The stale write, carrying the superseded resourceVersion, must be a
	// 409 — never a silent clobber of the second holder's entry.
	cm.Labels["stale-write-probe"] = "issue-69"
	_, err = cms.Update(t.Context(), cm, metav1.UpdateOptions{})
	if !kerrors.IsConflict(err) {
		t.Fatalf("update with stale resourceVersion: got %v, want 409 Conflict", err)
	}

	after := storedLockState(t, f)
	if _, ok := after.sharedHolders[f.holderID]; !ok {
		t.Errorf("stale update clobbered the lock state: first holder's entry is gone")
	}
	if _, ok := after.sharedHolders[f2.holderID]; !ok {
		t.Errorf("second holder's entry missing after rejected stale update")
	}
}

// TestIntegrationTwoInstancesRaceForReservedAndExclusive is the issue #69
// core: two VFS instances over one REAL apiserver race for RESERVED and for
// EXCLUSIVE. Whoever's compare-and-swap lands first wins; the loser re-reads
// (or 409s and re-reads) and finds the writer slot taken, reported as
// SQLITE_BUSY. Over several rounds the race interleaves differently; the
// invariant is that the writer slot is never double-held.
func TestIntegrationTwoInstancesRaceForReservedAndExclusive(t *testing.T) {
	v1 := newIntegrationVFS(t)
	v2 := newIntegrationVFS(t)

	type outcome struct {
		f   *file
		err error
	}

	for round := 0; round < 8; round++ {
		name := newSQLFileName()
		f1 := NewFile(name, v1)
		f2 := NewFile(name, v2)

		// Both instances take SHARED first: readers coexist, and both are
		// now live holders whose climb must be arbitrated.
		for i, f := range []*file{f1, f2} {
			if err := f.Lock(sqlite3vfs.LockShared); err != nil {
				t.Fatalf("round %d: instance %d Lock(SHARED): %v", round, i, err)
			}
		}

		// Race RESERVED.
		res := make(chan outcome, 2)
		go func() { res <- outcome{f1, f1.Lock(sqlite3vfs.LockReserved)} }()
		go func() { res <- outcome{f2, f2.Lock(sqlite3vfs.LockReserved)} }()
		var winners []*file
		for range 2 {
			o := <-res
			switch {
			case o.err == nil:
				winners = append(winners, o.f)
			case isBusyError(o.err):
				// The expected loser path: the writer slot was taken.
			default:
				t.Fatalf("round %d: Lock(RESERVED): unexpected error: %v", round, o.err)
			}
		}
		if len(winners) != 1 {
			t.Fatalf("round %d: RESERVED race: %d instances hold the writer, want exactly 1", round, len(winners))
		}
		winner := winners[0]
		st := storedLockState(t, winner)
		if st.writer == nil || st.writer.ID != winner.holderID {
			t.Fatalf("round %d: stored writer %+v, want the race winner %s", round, st.writer, winner.holderID)
		}
		if lvl, ok := st.writer.lockType(); !ok || lvl != sqlite3vfs.LockReserved {
			t.Fatalf("round %d: stored writer level %v, want LockReserved", round, st.writer.Level)
		}
		if len(st.sharedHolders) != 2 {
			t.Fatalf("round %d: shared holders %v, want both instances still present", round, st.sharedHolders)
		}

		// Release the round's file, then race EXCLUSIVE on a fresh one: the
		// RESERVED holder never reaches EXCLUSIVE while the other still holds
		// SHARED, so each round needs a clean start.
		if err := winner.Unlock(sqlite3vfs.LockNone); err != nil {
			t.Fatalf("round %d: winner unlock: %v", round, err)
		}
		loser := f1
		if winner == f1 {
			loser = f2
		}
		if err := loser.Unlock(sqlite3vfs.LockNone); err != nil {
			t.Fatalf("round %d: loser unlock: %v", round, err)
		}

		// Race EXCLUSIVE: both start from SHARED. The claim step (PENDING)
		// is single-winner; the acquire step stays BUSY until the other
		// holder's SHARED is gone, so both calls may report SQLITE_BUSY —
		// the cluster invariant is what the assertions check.
		f3 := NewFile(name, v1)
		f4 := NewFile(name, v2)
		for i, f := range []*file{f3, f4} {
			if err := f.Lock(sqlite3vfs.LockShared); err != nil {
				t.Fatalf("round %d: instance %d Lock(SHARED) for EXCLUSIVE race: %v", round, i, err)
			}
		}
		res = make(chan outcome, 2)
		go func() { res <- outcome{f3, f3.Lock(sqlite3vfs.LockExclusive)} }()
		go func() { res <- outcome{f4, f4.Lock(sqlite3vfs.LockExclusive)} }()
		for range 2 {
			o := <-res
			if o.err != nil && !isBusyError(o.err) {
				t.Fatalf("round %d: Lock(EXCLUSIVE): unexpected error: %v", round, o.err)
			}
		}

		// Exactly one PENDING/EXCLUSIVE writer exists, and both SHARED
		// entries survive (EXCLUSIVE was not reached: the other holder was
		// still live).
		st = storedLockState(t, f3)
		if st.writer == nil {
			t.Fatalf("round %d: EXCLUSIVE race left no writer entry: %+v", round, st)
		}
		if st.writer.ID != f3.holderID && st.writer.ID != f4.holderID {
			t.Fatalf("round %d: stored writer %s is neither instance", round, st.writer.ID)
		}
		if lvl, ok := st.writer.lockType(); !ok || (lvl != sqlite3vfs.LockPending && lvl != sqlite3vfs.LockExclusive) {
			t.Fatalf("round %d: stored writer level %q, want PENDING or EXCLUSIVE", round, st.writer.Level)
		}

		// The writer holder climbs once the other holder lets go; the loser
		// is refused SHARED while EXCLUSIVE is held.
		excl, blocked := f3, f4
		if st.writer.ID == f4.holderID {
			excl, blocked = f4, f3
		}
		if err := blocked.Unlock(sqlite3vfs.LockNone); err != nil {
			t.Fatalf("round %d: blocked holder unlock: %v", round, err)
		}
		if err := excl.Lock(sqlite3vfs.LockExclusive); err != nil {
			t.Fatalf("round %d: writer Lock(EXCLUSIVE) after other holder released: %v", round, err)
		}
		st = storedLockState(t, excl)
		if st.writer == nil || st.writer.ID != excl.holderID {
			t.Fatalf("round %d: writer entry %+v, want %s at EXCLUSIVE", round, st.writer, excl.holderID)
		}
		if lvl, _ := st.writer.lockType(); lvl != sqlite3vfs.LockExclusive {
			t.Fatalf("round %d: writer level %q, want LockExclusive", round, st.writer.Level)
		}
		if len(st.sharedHolders) != 0 {
			t.Fatalf("round %d: shared holders %v under EXCLUSIVE, want none", round, st.sharedHolders)
		}
		if err := blocked.Lock(sqlite3vfs.LockShared); !isBusyError(err) {
			t.Fatalf("round %d: SHARED under EXCLUSIVE: got %v, want SQLITE_BUSY", round, err)
		}
		if err := excl.Unlock(sqlite3vfs.LockNone); err != nil {
			t.Fatalf("round %d: EXCLUSIVE unlock: %v", round, err)
		}
	}

	t.Logf("real API server returned %d 409 Conflicts across the races", apiConflicts.Load())
}

// TestIntegrationConcurrentWritersViaDatabaseSQL is the fake-clientset
// concurrent-writers test (vfs_concurrent_test.go) replayed against the real
// apiserver: two processes' connections write concurrently through the real
// resourceVersion arbitration, and the database comes out consistent —
// PRAGMA integrity_check says ok and every committed row is present.
func TestIntegrationConcurrentWritersViaDatabaseSQL(t *testing.T) {
	v1 := newIntegrationVFS(t)
	v2 := newIntegrationVFS(t)
	vfs1Name := registerUniqueVFS(t, v1)
	vfs2Name := registerUniqueVFS(t, v2)
	fileName := newSQLFileName()

	dbA := openContendingSQLDB(t, vfs1Name, fileName)
	defer dbA.Close()
	dbB := openContendingSQLDB(t, vfs2Name, fileName)
	defer dbB.Close()

	// The schema exists before either writer goroutine starts, so the
	// concurrency under test is purely the two insert transactions.
	if _, err := dbA.Exec(`CREATE TABLE books (
		id text NOT NULL PRIMARY KEY,
		title text
	)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	const rowsPerWriter = 50
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		insertBatchWithBusyRetry(t, dbA, "a", rowsPerWriter)
	}()
	go func() {
		defer wg.Done()
		<-start
		insertBatchWithBusyRetry(t, dbB, "b", rowsPerWriter)
	}()
	close(start)
	wg.Wait()
	if t.Failed() {
		return
	}

	want := expectedBooks("a", rowsPerWriter)
	for id, title := range expectedBooks("b", rowsPerWriter) {
		want[id] = title
	}
	assertDatabaseConsistent(t, dbA, want)

	t.Logf("real API server returned %d 409 Conflicts during the concurrent writes", apiConflicts.Load())
}

// TestIntegrationStaleHolderLeaseLapsesAndIsPruned drives issue #47's lease
// takeover against the real apiserver: a holder whose clock is skewed into
// the past stamps entries that are ALREADY expired when stored; the next
// CAS of a live holder prunes them and takes the lock, and the stale holder
// discovers the loss on its next operation — a read/write fails with
// SQLITE_BUSY and the next Lock reports the distinct lease-loss error after
// resetting its local state.
func TestIntegrationStaleHolderLeaseLapsesAndIsPruned(t *testing.T) {
	v1 := newIntegrationVFS(t)
	f1 := NewFile(newSQLFileName(), v1)
	if err := f1.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("stale holder Lock(SHARED): %v", err)
	}
	if n, err := f1.WriteAt([]byte("hello"), 0); err != nil || n != 5 {
		t.Fatalf("stale holder WriteAt: n=%d err=%v, want n=5 err=nil", n, err)
	}

	// Let the holder's lease lapse the way the cluster sees a lapsed lease:
	// its stored expiry moves into the past (a holder gone silent — crashed,
	// partitioned — while its entries stay behind). The patch is one raw
	// Update of the shared-holders map, everything else untouched.
	expireHolderLease(t, f1)
	st := storedLockState(t, f1)
	if expiry, ok := st.sharedHolders[f1.holderID]; !ok || !expiry.Before(time.Now()) {
		t.Fatalf("stale holder's stored lease %v (present=%t), want an expired timestamp", expiry, ok)
	}

	// A live holder's CAS reads the state, prunes the expired entry and
	// takes the lock in one resourceVersion-checked write.
	v2 := newIntegrationVFS(t)
	f2 := NewFile(f1.RawName, v2)
	if err := f2.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("live holder Lock(SHARED) over a lapsed lease: %v", err)
	}

	st = storedLockState(t, f2)
	if _, ok := st.sharedHolders[f1.holderID]; ok {
		t.Fatalf("lapsed holder %s still stored after pruning: %v", f1.holderID, st.sharedHolders)
	}
	expiry, ok := st.sharedHolders[f2.holderID]
	if !ok || !expiry.After(time.Now()) {
		t.Fatalf("live holder's stored lease %v (present=%t), want a future expiry", expiry, ok)
	}

	// The stale holder's next renewal discovers the loss. Locally its lease
	// still looks fresh (its clock never saw the lapse), so the test forces
	// the renewal decision the way real elapsed time would: leaseExpires at
	// the zero time makes maybeRenew due on the next lock-path call. The
	// lease error is deliberately NOT SQLITE_BUSY (retrying the same request
	// would run over a database another process may already have rewritten).
	f1.leaseExpires = time.Time{}
	err := f1.Lock(sqlite3vfs.LockShared)
	if !errors.Is(err, errLockLeaseLost) {
		t.Fatalf("stale holder Lock after its lease was pruned: got %v, want the lease-loss error", err)
	}
	if isBusyError(err) {
		t.Fatalf("lease loss reported as SQLITE_BUSY (%v): sqlite would retry the very same request", err)
	}

	// And the sector read/write path refuses from then on — the state this
	// file cached under the lost lock may already be rewritten.
	if _, err := f1.WriteAt([]byte("world"), 10); !isBusyError(err) {
		t.Fatalf("stale holder WriteAt after lease loss: got %v, want SQLITE_BUSY", err)
	}
}

// expireHolderLease rewrites the holder's stored lease to the past, exactly
// what a lapsed lease looks like on the cluster: the entry is there, but its
// expiry is behind every other process's clock.
func expireHolderLease(t *testing.T, f *file) {
	t.Helper()
	cms := testKc.CoreV1().ConfigMaps(testNamespace)
	cm, err := cms.Get(t.Context(), f.LockFileName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get lock ConfigMap to expire the lease: %v", err)
	}
	var holders map[string]time.Time
	if err := json.Unmarshal([]byte(cm.Data[sharedHoldersDataKey]), &holders); err != nil {
		t.Fatalf("parse stored holders: %v", err)
	}
	holders[f.holderID] = time.Now().Add(-10 * time.Minute)
	raw, err := json.Marshal(holders)
	if err != nil {
		t.Fatalf("marshal holders: %v", err)
	}
	cm.Data[sharedHoldersDataKey] = string(raw)
	if _, err := cms.Update(t.Context(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("expire holder lease on the cluster: %v", err)
	}
}

// TestIntegrationOnClusterLayout asserts the on-cluster data layout exactly
// as the API server stores it: sector ConfigMaps named and labelled as the
// README documents, the lockfile's aggregate/holder/writer keys, a file that
// spans several 64 KiB sectors, and a Delete that removes all of it.
func TestIntegrationOnClusterLayout(t *testing.T) {
	v := newIntegrationVFS(t)
	name := newSQLFileName()
	f := NewFile(name, v)
	b32 := b32Name(name)
	label := wantRelevantFileLabel(name)

	// Names: <base32 filename>-lockfile and <base32 filename>-<index>.
	if want := b32 + "-lockfile"; f.LockFileName() != want {
		t.Fatalf("lockfile name %q, want %q", f.LockFileName(), want)
	}

	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(SHARED): %v", err)
	}
	cm := getLockConfigMap(t, f)

	// Lockfile labels: data=lockfile plus relevant-file=<the file's label —
	// the base32 filename while it fits the 63-character label-value cap,
	// here the short test name, so the label is the encoding itself
	// (issue #351)>.
	wantLabels := map[string]string{"data": "lockfile", "relevant-file": label}
	if cm.Name != b32+"-lockfile" {
		t.Errorf("stored lockfile name %q, want %q", cm.Name, b32+"-lockfile")
	}
	for k, want := range wantLabels {
		if got := cm.Labels[k]; got != want {
			t.Errorf("lockfile label %q = %q, want %q", k, got, want)
		}
	}

	// Lockfile data: the aggregate level, the relevant-file key, the holder
	// map with a future lease, and no writer yet.
	if got := cm.Data[lockLevelDataKey]; got != sqlite3vfs.LockShared.String() {
		t.Errorf("stored %s = %q, want %q", lockLevelDataKey, got, sqlite3vfs.LockShared.String())
	}
	if got := cm.Data["relevant-file"]; got != b32 {
		t.Errorf("stored relevant-file = %q, want %q", got, b32)
	}
	var holders map[string]time.Time
	if err := json.Unmarshal([]byte(cm.Data[sharedHoldersDataKey]), &holders); err != nil {
		t.Fatalf("stored %s is not a holder map: %v", sharedHoldersDataKey, err)
	}
	if expiry, ok := holders[f.holderID]; !ok || !expiry.After(time.Now()) {
		t.Errorf("stored holder lease for this file = %v (present=%t), want a future expiry", expiry, ok)
	}
	if cm.Data[writerDataKey] != "" {
		t.Errorf("stored %s = %q, want empty before any writer climbs", writerDataKey, cm.Data[writerDataKey])
	}

	// Climb to RESERVED: the writer entry appears with id, level and lease.
	if err := f.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("Lock(RESERVED): %v", err)
	}
	cm = getLockConfigMap(t, f)
	var w writerEntry
	if err := json.Unmarshal([]byte(cm.Data[writerDataKey]), &w); err != nil {
		t.Fatalf("stored %s is not a writer entry: %v", writerDataKey, err)
	}
	if w.ID != f.holderID || w.Level != sqlite3vfs.LockReserved.String() || !w.Lease.After(time.Now()) {
		t.Errorf("stored writer = %+v, want this holder at LockReserved with a future lease", w)
	}

	// A 160 KiB write spans three 64 KiB sectors: 0, 1 and 2, each a
	// ConfigMap with the sector bytes in BinaryData and the raw file name in
	// the "filename" key.
	const fileSize = 160 * 1024
	buf := make([]byte, fileSize)
	for i := range buf {
		buf[i] = byte(i % 251)
	}
	if n, err := f.WriteAt(buf, 0); err != nil || n != fileSize {
		t.Fatalf("WriteAt %d bytes: n=%d err=%v", fileSize, n, err)
	}
	sectorList, err := testKc.CoreV1().ConfigMaps(testNamespace).List(t.Context(), metav1.ListOptions{
		LabelSelector: "data=sector,relevant-file=" + label,
	})
	if err != nil {
		t.Fatalf("list sector ConfigMaps: %v", err)
	}
	got := map[string]corev1.ConfigMap{}
	for _, s := range sectorList.Items {
		got[s.Name] = s
	}
	if len(got) != 3 {
		t.Fatalf("label-selector LIST returned %d ConfigMaps, want 3 sectors", len(got))
	}
	for idx, wantLen := range map[int]int{0: 64 * 1024, 1: 64 * 1024, 2: 32 * 1024} {
		sectorName := fmt.Sprintf("%s-%d", b32, idx)
		s, ok := got[sectorName]
		if !ok {
			t.Fatalf("sector ConfigMap %q missing from the cluster", sectorName)
		}
		if label := s.Labels["data"]; label != "sector" {
			t.Errorf("sector %s label data=%q, want \"sector\"", sectorName, label)
		}
		if fn := s.Data["filename"]; fn != name {
			t.Errorf("sector %s filename = %q, want %q", sectorName, fn, name)
		}
		if data := s.BinaryData["sector"]; len(data) != wantLen {
			t.Errorf("sector %s holds %d bytes, want %d", sectorName, len(data), wantLen)
		} else if idx == 0 && string(data[:5]) != string(buf[:5]) {
			t.Errorf("sector 0 does not start with the written bytes")
		}
	}

	// The VFS's own sector LIST agrees on the file size (the lockfile also
	// carries relevant-file, so the raw LIST above selects data=sector).
	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	if size != fileSize {
		t.Errorf("FileSize = %d, want %d", size, fileSize)
	}

	// Delete removes every sector ConfigMap and the lockfile.
	if err := f.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := v.Delete(name, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	after, err := testKc.CoreV1().ConfigMaps(testNamespace).List(t.Context(), metav1.ListOptions{
		LabelSelector: "data=sector,relevant-file=" + label,
	})
	if err != nil {
		t.Fatalf("post-delete list: %v", err)
	}
	if len(after.Items) != 0 {
		t.Errorf("post-delete LIST returned %d ConfigMaps, want 0", len(after.Items))
	}
	if _, err := testKc.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), f.LockFileName(), metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Errorf("post-delete lockfile: got %v, want NotFound", err)
	}
}

// TestIntegrationLongNameWritesOnRealAPIServer pins the fix for issue #351
// exactly where the bug showed: longNameDB is 40 bytes, its base32 encoding
// 64 characters — one past the 63-character label-value cap — so on a real
// API server the lockfile Create inside Open and every sector write used to
// come back 422 Invalid, and such a database could not be opened for
// writing at all. With the label shortened to the "long-" stand-in, Open,
// a multi-sector write, the label-selector LIST, the size computation and
// Delete all succeed against the real API server, and no stored object
// carries a label value over 63 characters.
func TestIntegrationLongNameWritesOnRealAPIServer(t *testing.T) {
	v := newIntegrationVFS(t)
	name := longNameDB
	wantLabel := wantRelevantFileLabel(name)

	sf, _, err := v.Open(name, 0)
	if err != nil {
		t.Fatalf("Open(%q) on the real API server: %v", name, err)
	}
	f, ok := sf.(*file)
	if !ok {
		t.Fatalf("Open(%q) returned %T, want *file", name, sf)
	}

	cms := testKc.CoreV1().ConfigMaps(testNamespace)
	lf, err := cms.Get(t.Context(), f.LockFileName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the lockfile the real API server just accepted: %v", err)
	}
	if got := lf.Labels["relevant-file"]; got != wantLabel {
		t.Errorf("lockfile relevant-file label = %q, want %q", got, wantLabel)
	}
	if got := lf.Data["relevant-file"]; got != b32Name(name) {
		t.Errorf("lockfile relevant-file data key = %q, want the full encoded name %q", got, b32Name(name))
	}
	for _, l := range lf.Labels {
		if len(l) > 63 {
			t.Errorf("lockfile label %q is %d characters, want at most 63 (the API server accepts it, so it must not)", l, len(l))
		}
	}

	// A 160 KiB write spans three sectors, every one a Create the real API
	// server must also accept.
	const fileSize = 160 * 1024
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(SHARED): %v", err)
	}
	buf := make([]byte, fileSize)
	fillPattern(buf)
	if n, err := f.WriteAt(buf, 0); err != nil || n != fileSize {
		t.Fatalf("WriteAt %d bytes on the real API server: n=%d err=%v", fileSize, n, err)
	}
	sectorList, err := cms.List(t.Context(), metav1.ListOptions{
		LabelSelector: "data=sector,relevant-file=" + wantLabel,
	})
	if err != nil {
		t.Fatalf("label-selector LIST with the shortened label: %v", err)
	}
	if len(sectorList.Items) != 3 {
		t.Fatalf("LIST on the shortened label returned %d ConfigMaps, want the 3 sectors", len(sectorList.Items))
	}
	for _, s := range sectorList.Items {
		if got := s.Labels["relevant-file"]; got != wantLabel {
			t.Errorf("sector %s label = %q, want %q", s.Name, got, wantLabel)
		}
		if fn := s.Data["filename"]; fn != name {
			t.Errorf("sector %s filename = %q, want %q", s.Name, fn, name)
		}
	}
	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	if size != fileSize {
		t.Errorf("FileSize = %d, want %d", size, fileSize)
	}
	readBack := make([]byte, fileSize)
	if n, err := f.ReadAt(readBack, 0); err != nil || n != fileSize {
		t.Fatalf("ReadAt %d bytes: n=%d err=%v", fileSize, n, err)
	}
	if !bytes.Equal(readBack, buf) {
		t.Errorf("read back different bytes than were written")
	}

	if err := f.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := v.Delete(name, false); err != nil {
		t.Fatalf("Delete on the real API server: %v", err)
	}
	after, err := cms.List(t.Context(), metav1.ListOptions{
		LabelSelector: "data=sector,relevant-file=" + wantLabel,
	})
	if err != nil {
		t.Fatalf("post-delete LIST: %v", err)
	}
	if len(after.Items) != 0 {
		t.Errorf("post-delete LIST returned %d ConfigMaps, want 0", len(after.Items))
	}
	if _, err := cms.Get(t.Context(), f.LockFileName(), metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Errorf("post-delete lockfile get: err=%v, want NotFound", err)
	}
}
