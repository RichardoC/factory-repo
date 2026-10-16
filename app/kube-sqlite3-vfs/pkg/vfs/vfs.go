// Package vfs implements a sqlite3vfs virtual file system that stores
// database files in Kubernetes as ConfigMaps: one ConfigMap per 64 KiB
// sector plus a "lockfile" ConfigMap holding the SQLite lock level.
//
// A database file is split into fixed-size sectors (SectorSize — 64 KiB, the
// largest SQLite page size). Each sector is the BinaryData of a ConfigMap
// named "<base32 filename>-<sector index>", labelled with the file's base32
// name; the lock ConfigMap is named "<base32 filename>-lockfile" and holds
// the lock level as a string. Reads and writes map onto Get/Update/Create
// calls, and the file size is computed from a single LIST of the file's
// sector ConfigMaps.
//
// The VFS talks to any kubernetes.Interface: pass a real clientset in
// production and client-go's fake clientset in tests. The context given to
// NewVFS is used for every Kubernetes call the VFS makes; SQLite calls into
// the VFS through cgo, so there is no per-call context to accept.
package vfs

import (
	"context"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strings"

	"github.com/psanford/sqlite3vfs"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// LockFileNameSuffix is appended to the base32-encoded file name to form
	// the name of the ConfigMap holding the file's SQLite lock level.
	LockFileNameSuffix = "lockfile"

	// SectorSize is the size of one stored sector: 64 KiB, the largest page
	// size SQLite supports. Each sector is stored as one ConfigMap.
	SectorSize = 64 * 1024 // Max pagesize supported by SQLITE3
)

// CommonSectorLabel and LockfileLabel are the base labels every sector and
// lockfile ConfigMap, respectively, carries. They are vars only because Go
// cannot express a map as a const.
var (
	CommonSectorLabel = map[string]string{"data": "sector"}
	LockfileLabel     = map[string]string{"data": "lockfile"}
)

type vfs struct {
	kc        kubernetes.Interface
	logger    *slog.Logger
	retries   int
	namespace string

	// ctx backs every Kubernetes call this VFS makes. It is fixed at
	// construction because SQLite enters the VFS through cgo, where there
	// is no caller context to propagate.
	ctx context.Context
}

// NewVFS returns a VFS that stores database files as ConfigMaps in
// namespace. Every Kubernetes call the VFS makes uses ctx. retries is the
// number of attempts Open makes against races between concurrent clients.
func NewVFS(ctx context.Context, kc kubernetes.Interface, namespace string, logger *slog.Logger, retries int) *vfs {
	return &vfs{kc: kc, logger: logger, retries: retries, namespace: namespace, ctx: ctx}
}

func (f *file) b32ByteFromString(s string) []byte {
	sb := []byte(s)
	dst := make([]byte, f.encoding.EncodedLen(len(sb)))
	f.encoding.Encode(dst, sb)
	return dst
}

// Close implements sqlite3vfs.File: it releases the file's lock by writing
// LockNone to the lock ConfigMap.
func (f *file) Close() error {
	return f.setLock(sqlite3vfs.LockNone)
}

// Truncate implements sqlite3vfs.File: it shortens the file to size bytes,
// deleting the sectors past the new end and shortening the one holding the
// kept prefix. Growing (or no-op) truncates do nothing.
func (f *file) Truncate(size int64) error {
	fileSize, err := f.FileSize()
	if err != nil {
		return err
	}

	if size >= fileSize {
		return nil
	}

	// The last surviving byte is at size-1; the sector holding it may need
	// shortening. (The imported code used sectorForPos(size), which for a
	// sector-aligned size points one sector PAST the surviving data.)
	// Truncating to 0 leaves no surviving byte at all: every sector goes.
	lastRemainingSector := int64(-1)
	if size > 0 {
		lastRemainingSector = f.sectorForPos(size - 1)
	}

	if lastRemainingSector >= 0 && size%SectorSize != 0 {
		sect, err := f.getSector(lastRemainingSector)
		if err != nil && !errors.Is(err, errSectorNotFound) {
			return err
		}
		if err == nil {
			keep := size % SectorSize
			if int64(len(sect.Data)) > keep {
				sect.Data = sect.Data[:keep]
			} else if int64(len(sect.Data)) < keep {
				// Inconsistent state (the sector holds less than the file
				// size claimed): zero-pad so the kept prefix has room.
				padded := make([]byte, keep)
				copy(padded, sect.Data)
				sect.Data = padded
			}
			// BUG FIX (issue #8): the imported code ignored this error and
			// carried on deleting sectors, silently losing the shortened
			// sector's data.
			if err := f.writeSectorOnce(sect.Index, sect.Data, true); err != nil {
				return err
			}
			if f.valid {
				f.cacheSector(sect.Index, sect.Data)
			}
		}
		// errSectorNotFound: the sector holding the new last byte does not
		// exist (a hole in a sparse file), so there is nothing to shorten.
	}

	lastSector := f.sectorForPos(fileSize)

	// BUG FIX (issue #8): the delete loop used to START at
	// lastRemainingSector, deleting the sector that holds the kept prefix
	// (and, when fileSize was sector-aligned, running past the last
	// existing sector). It must start after it.
	for sectToDelete := lastRemainingSector + 1; sectToDelete <= lastSector; sectToDelete++ {
		err := f.deleteSector(sectToDelete)
		if err != nil && !kerrors.IsNotFound(err) {
			return err
		}
		if f.valid {
			delete(f.sectors, sectToDelete)
		}
	}

	if f.valid {
		f.size = size
	}

	return nil
}

// FileSize returns the size of the file. While the cache is valid (a lock
// of SHARED or better is held) this is O(1): the size is served from memory,
// having been observed from the cluster — or written by this process — since
// the lock was acquired. Otherwise it costs one LIST of the file's sector
// ConfigMaps; the imported version LISTed and then GET the last sector on
// EVERY call, including once per read.
//
// When the lock is held, that same LIST response warms the sector cache:
// this is the revalidation required after (re)acquiring SHARED, because
// another process may have modified the database between our locks.
func (f *file) FileSize() (int64, error) {
	f.vfs.logger.Debug("FileSize", "f", f)
	if f.valid {
		return f.size, nil
	}
	cms, err := f.listSectorConfigMaps()
	if err != nil {
		f.vfs.logger.Error("FileSize: list sector configmaps", "err", err)

		return 0, err
	}
	size := f.sizeFromSectorList(cms)
	if f.lockKnown && f.lockLevel >= sqlite3vfs.LockShared {
		f.size = size
		f.valid = true
		f.sectors = make(map[int64][]byte, len(cms))
		for i := range cms {
			if idx, ok := f.sectorIndexFromConfigMapName(cms[i].Name); ok {
				data := make([]byte, len(cms[i].BinaryData["sector"]))
				copy(data, cms[i].BinaryData["sector"])
				f.cacheSector(idx, data)
			}
		}
	}
	f.vfs.logger.Debug("FileSize", "f", f, "size", size)

	return size, nil
}

// A file object backs one sqlite connection; sqlite serialises the calls
// made per connection, so (as before this change) no extra synchronisation
// is added around the fields below.
type file struct {
	// RawName is the unencoded name the file was opened under.
	RawName string

	vfs      *vfs
	encoding *base32.Encoding

	// SectorLabels are the labels every sector ConfigMap of this file
	// carries: CommonSectorLabel plus the file's base32 "relevant-file".
	SectorLabels map[string]string

	// Local view of the shared state, kept to cut Kubernetes API round
	// trips. Everything cached here is trusted only while a lock of SHARED
	// or better is held: another process may hold and modify the database
	// between our locks, so the caches are dropped the moment the lock
	// drops below SHARED (adoptLockState) and revalidated with one LIST by
	// the first FileSize after SHARED is re-acquired.
	//
	// lockLevel/lockKnown: the lock level this process believes IT holds.
	// sqlite asks Lock, Unlock and CheckReservedLock around every
	// operation; the imported code GET the lock ConfigMap before each one.
	// A file opened through the VFS starts at NONE and only ever records
	// levels it wrote itself — the ConfigMap's stored level is never
	// adopted as this file's own, because it may be another process's
	// lock. A file object created with NewFile (the cmd tools) has no known
	// local state and consults the ConfigMap per transition, as before.
	//
	// size/valid/sectors: the file size and sector contents last observed
	// from the cluster, or written by this process, while the lock is held.
	// valid is only ever true while lockLevel >= LockShared.
	lockLevel sqlite3vfs.LockType
	lockKnown bool
	size      int64
	valid     bool
	sectors   map[int64][]byte
}

// ReadAt implements sqlite3vfs.File. A read that starts at or past the end
// of the file returns (0, io.EOF); one that reaches past the end returns
// the bytes read so far with io.EOF, which the sqlite3vfs layer turns into
// SQLITE_IOERR_SHORT_READ.
func (f *file) ReadAt(p []byte, off int64) (int, error) {
	f.vfs.logger.Debug("ReadAt", "f", f, "off", off, "len(buffer)", len(p))

	firstSector := f.sectorForPos(off)

	fileSize, err := f.FileSize()
	if err != nil {
		f.vfs.logger.Debug("ReadAt", "off", off, "len(buffer)", len(p), "fileSize", fileSize, "err", err)

		return 0, err
	}

	lastByte := off + int64(len(p)) - 1

	lastSector := f.sectorForPos(lastByte)
	if lastByte > fileSize {
		lastSector = f.sectorForPos(fileSize)
		lastByte = fileSize
	}
	if off >= fileSize {
		// Maybe this will stop it being unhappy about the journal file not existing?
		err = io.EOF
		f.vfs.logger.Debug("ReadAt", "off", off, "len(buffer)", len(p), "fileSize", fileSize, "err", err)
		return 0, err
	}

	var (
		n     int
		first = true
	)
	sectors, err := f.getSectorRange(firstSector, lastSector)
	if err != nil {
		return 0, sqlite3vfs.IOErrorRead
	}
	for _, sect := range sectors {
		if first {
			startIndex := off % SectorSize
			// A sector that does not exist (a hole) contributes no bytes:
			// guard the slice so this reports a short read instead of
			// panicking. The imported code created the missing sector on
			// read (a write to the cluster!) and then panicked anyway.
			if int(startIndex) < len(sect.Data) {
				n = copy(p, sect.Data[startIndex:])
			}
			first = false
			continue
		}

		nn := copy(p[n:], sect.Data)
		n += nn
	}
	if lastByte >= fileSize {
		return n, io.EOF
	}
	f.vfs.logger.Debug("ReadAt", "off", off, "len(buffer)", len(p), "n", n)

	if n < len(p) {
		f.vfs.logger.Error("Read too little")
		return n, io.EOF
	}

	f.vfs.logger.Debug("ReadAt read all expected bytes")

	return n, nil
}

// WriteAt implements sqlite3vfs.File: it writes p at off, fetching each
// touched sector at most once and writing each changed sector exactly once
// (see writeSectorOnce). Sectors past the current end are created, so a
// write grows the file across as many 64 KiB sectors as it needs.
func (f *file) WriteAt(p []byte, off int64) (int, error) {
	f.vfs.logger.Debug("WriteAt", "len(p)", len(p), "off", off)

	if len(p) == 0 {
		return 0, nil
	}

	firstSector := f.sectorForPos(off)

	lastByte := off + int64(len(p)) - 1

	lastSector := f.sectorForPos(lastByte)

	var nW int
	for sectorIndex := firstSector; sectorIndex <= lastSector; sectorIndex++ {
		// Fetch each sector's current bytes at most once (a cache hit while
		// the lock is held, otherwise one GET); a sector that does not exist
		// yet is created by the write below rather than by the fetch. The
		// imported code fetched every sector up front via getSector, which
		// created missing sectors on the cluster and then panicked on the
		// nil response it got back — so no write could ever grow a file.
		data, exists, err := f.sectorDataForWrite(sectorIndex)
		if err != nil {
			return nW, sqlite3vfs.IOErrorRead
		}

		lastPossibleByteForThisSector := ((sectorIndex + 1) * SectorSize) - 1
		startByteForThisSector := sectorIndex * SectorSize

		currentOffset := off + int64(nW)
		var sectorData []byte
		if lastByte > lastPossibleByteForThisSector {
			sectorData = make([]byte, SectorSize)
		} else {
			// If there's existing data, ensure the buffer is large enough to hold it
			newlyRequiredSize := (1 + lastByte) - startByteForThisSector
			newlyRequiredSize = max(newlyRequiredSize, int64(len(data)))
			sectorData = make([]byte, newlyRequiredSize)
		}
		startOffset := currentOffset - startByteForThisSector
		// copy in existing data
		_ = copy(sectorData, data)
		// copy new data
		nn := copy(sectorData[startOffset:], p[nW:])
		// Write the sector exactly once: UPDATE when it is known to exist,
		// CREATE when it is known to be new; an unexpected conflict falls
		// back to the other verb (see writeSectorOnce).
		if err := f.writeSectorOnce(sectorIndex, sectorData, exists); err != nil {
			f.vfs.logger.Error("WriteAt: write sector", "sector", sectorIndex, "err", err)
			return nW, err
		}
		if f.valid {
			f.cacheSector(sectorIndex, sectorData)
			if end := sectorIndex*SectorSize + int64(len(sectorData)); end > f.size {
				f.size = end
			}
		}
		nW += nn
	}

	return nW, nil
}

// Sync implements sqlite3vfs.File: it no-ops, because sector writes reach
// the cluster directly.
func (f *file) Sync(flag sqlite3vfs.SyncType) error {
	f.vfs.logger.Debug("Sync", "flag", flag)

	return nil
}

// lockLevelFromString maps the lock ConfigMap's "lock" value to a
// sqlite3vfs.LockType, reporting false for an unknown value.
func lockLevelFromString(currentLockString string) (sqlite3vfs.LockType, bool) {
	switch currentLockString {
	case sqlite3vfs.LockNone.String():
		return sqlite3vfs.LockNone, true
	case sqlite3vfs.LockShared.String():
		return sqlite3vfs.LockShared, true
	case sqlite3vfs.LockPending.String():
		return sqlite3vfs.LockPending, true
	case sqlite3vfs.LockExclusive.String():
		return sqlite3vfs.LockExclusive, true
	case sqlite3vfs.LockReserved.String():
		return sqlite3vfs.LockReserved, true
	}
	return sqlite3vfs.LockNone, false
}

func (f *file) getCurrentLock() (sqlite3vfs.LockType, error) {
	f.vfs.logger.Debug("getCurrentLock")

	cm, err := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Get(f.vfs.ctx, f.LockFileName(), metav1.GetOptions{})
	if err != nil {
		return sqlite3vfs.LockNone, fmt.Errorf("get lock configmap: %w", err)
	}
	lockToReturn, ok := lockLevelFromString(cm.Data["lock"])
	if !ok {
		return sqlite3vfs.LockNone, fmt.Errorf("lock type unknown: file %v, configmap lock value %q", f, cm.Data["lock"])
	}
	f.vfs.logger.Debug("getCurrentLock", "found current lock", lockToReturn)

	return lockToReturn, nil
}

// currentLockLevel returns the lock level to judge a transition against.
// While this file's state is known locally — every file opened through the
// VFS starts at NONE and records its own transitions — no API call is made:
// re-GETting the ConfigMap before every Lock/Unlock, as the imported code
// did, is one of the per-operation API calls this optimisation removes. A
// file object created with NewFile (the cmd tools) has no local state and
// consults the ConfigMap here, as before.
//
// The fetched level is deliberately NOT recorded as this file's own: it may
// be another process's lock, and believing it ours is what let a newly
// opened file skip its own transitions and clobber the cluster's level
// under them. Local state is only ever written by adoptLockState, after
// this process's own setLock succeeded.
func (f *file) currentLockLevel() (sqlite3vfs.LockType, error) {
	f.vfs.logger.Debug("currentLockLevel")
	if f.lockKnown {
		return f.lockLevel, nil
	}
	return f.getCurrentLock()
}

// LockFileName returns the name of the ConfigMap holding this file's SQLite
// lock level: the base32-encoded file name plus LockFileNameSuffix.
func (f *file) LockFileName() string {
	localLockFileName := fmt.Sprintf("%s-%s", f.b32ByteFromString(f.RawName), LockFileNameSuffix)
	return localLockFileName
}

func (f *file) generateSectorsLabels() {
	fileNameLabel := string(f.b32ByteFromString(f.RawName))

	f.SectorLabels = make(map[string]string, len(CommonSectorLabel)+1)
	maps.Copy(f.SectorLabels, CommonSectorLabel)
	f.SectorLabels["relevant-file"] = fileNameLabel
}

// setLock writes the requested lock level to the lock ConfigMap, creating
// the ConfigMap if it does not exist yet, and records the transition
// locally (adoptLockState) only once the write succeeded.
func (f *file) setLock(lock sqlite3vfs.LockType) error {
	f.vfs.logger.Debug("setLock", "lock", lock)

	// Any downgrade — Unlock and Close land here — invalidates the local
	// view BEFORE the write is attempted, whatever the write returns. If
	// the ConfigMap write fails transiently, sqlite largely ignores the
	// unlock error and carries on, so the caches must already be gone by
	// then: with the lock state unknown, the next Lock/Unlock/
	// CheckReservedLock re-GETs the ConfigMap and the next FileSize pays
	// its revalidating LIST, so writes another process made between our
	// locks can never be masked by stale cached sectors. On success,
	// adoptLockState below records the new level (dropping the caches
	// again for sub-SHARED levels, a no-op by then).
	if f.lockKnown && lock < f.lockLevel {
		f.dropCaches()
		f.lockKnown = false
	}

	fileNameLabel := string(f.b32ByteFromString(f.RawName))
	lockfileLabels := make(map[string]string, len(LockfileLabel)+1)
	maps.Copy(lockfileLabels, LockfileLabel)
	lockfileLabels["relevant-file"] = fileNameLabel

	lf := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: f.LockFileName(), Labels: lockfileLabels}, Data: map[string]string{"lock": lock.String(), "relevant-file": fileNameLabel}}

	_, err := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Update(f.vfs.ctx, lf, metav1.UpdateOptions{})
	f.vfs.logger.Debug("setLock", "lock", lock, "err", err)
	if kerrors.IsNotFound(err) {
		_, err = f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Create(f.vfs.ctx, lf, metav1.CreateOptions{})
		f.vfs.logger.Debug("setLock has been created", "lock", lock, "err", err)
	}
	if err == nil {
		f.adoptLockState(lock)
	}

	return err
}

// dropCaches discards the cached file size and sector contents. Everything
// cached is trusted only while this process holds a lock of SHARED or
// better; from the moment that stops being true the cache may not match the
// cluster, and the next FileSize revalidates it with one LIST after SHARED
// is re-acquired.
func (f *file) dropCaches() {
	f.valid = false
	f.sectors = nil
}

// adoptLockState records a lock transition this process just made, and
// drops the sector/size caches when the lock falls below SHARED: from that
// moment another process may modify the database, so everything cached
// under the lock is stale until the next FileSize revalidates it after
// SHARED is re-acquired.
func (f *file) adoptLockState(lock sqlite3vfs.LockType) {
	f.lockLevel = lock
	f.lockKnown = true
	if lock < sqlite3vfs.LockShared {
		f.dropCaches()
	}
}

// Lock implements sqlite3vfs.File: it acquires or upgrades the file's lock
// through the lock ConfigMap. Only the transitions of the sqlite locking
// chart below are legal; sqlite never requests PENDING explicitly, and a
// file holding no lock cannot climb past SHARED in one step.
func (f *file) Lock(elock sqlite3vfs.LockType) error {
	f.vfs.logger.Debug("Lock", "elock", elock)
	currentLock, err := f.currentLockLevel()
	if err != nil {
		f.vfs.logger.Error("Lock", "err", err)
		return err
	}

	//    UNLOCKED -> SHARED
	//    SHARED -> RESERVED
	//    SHARED -> (PENDING) -> EXCLUSIVE
	//    RESERVED -> (PENDING) -> EXCLUSIVE
	//    PENDING -> EXCLUSIVE

	if elock <= currentLock {
		return nil
	}

	//  (1) We never move from unlocked to anything higher than shared lock.
	if currentLock == sqlite3vfs.LockNone && elock > sqlite3vfs.LockShared {
		return errors.New("invalid lock transition requested")
	}
	//  (2) SQLite never explicitly requests a pending lock.
	if elock == sqlite3vfs.LockPending {
		return errors.New("invalid Lock() request for state pending")
	}
	//  (3) A shared lock is always held when a reserve lock is requested.
	if elock == sqlite3vfs.LockReserved && currentLock != sqlite3vfs.LockShared {
		return errors.New("can only transition to Reserved lock from Shared lock")
	}

	return f.setLock(elock)
}

// Unlock implements sqlite3vfs.File: it drops or downgrades the file's lock
// through the lock ConfigMap. An unlock above SHARED (sqlite never unlocks
// upward) or to a level above the one currently held is an error, not a
// panic: the sqlite3vfs interface lets Unlock report an error, and a panic
// here crashed the whole process through cgo.
func (f *file) Unlock(elock sqlite3vfs.LockType) error {
	f.vfs.logger.Debug("Unlock", "elock", elock)

	currentLock, err := f.currentLockLevel()
	if err != nil {
		f.vfs.logger.Error("Unlock", "err", err)
		return err
	}

	if elock > sqlite3vfs.LockShared {
		return fmt.Errorf("invalid unlock request to level %s", elock)
	}

	if currentLock < elock {
		return fmt.Errorf("cannot unlock to a level %s above the current lock level %s", elock, currentLock)
	}

	if elock == currentLock {
		return nil
	}

	if elock == sqlite3vfs.LockShared {
		return f.setLock(sqlite3vfs.LockShared)
	}

	return f.setLock(sqlite3vfs.LockNone)
}

// SectorSize implements sqlite3vfs.File: it reports the size of one stored
// sector, SectorSize (64 KiB).
func (f *file) SectorSize() int64 {
	return SectorSize
}

// DeviceCharacteristics implements sqlite3vfs.File: it advertises atomic
// 64 KiB writes, since one sector is one ConfigMap write.
func (f *file) DeviceCharacteristics() sqlite3vfs.DeviceCharacteristic {
	return sqlite3vfs.IocapAtomic64K
}

// NewFile returns a file object for name backed by v. The cmd tools use it
// to read and write a database stored in the cluster without going through
// a sqlite connection.
func NewFile(name string, v *vfs) *file {
	o := base32.NewEncoding("abcdefghijklmnopqrstuv0123456789")
	e := o.WithPadding('x')
	f := &file{RawName: name, vfs: v, encoding: e}
	f.generateSectorsLabels()
	return f
}

// Open implements sqlite3vfs.VFS: it backs name with the cluster, creating
// the file's lock ConfigMap at LockNone and an empty sector 0 if the file
// has no sectors yet. The lock level stored in the ConfigMap may belong to
// another process, so a file opened here starts at NONE locally and records
// only the transitions it performs itself (see file.lockLevel). Transient
// races between clients are retried up to the VFS's retry budget.
func (v *vfs) Open(name string, flags sqlite3vfs.OpenFlag) (sqlite3vfs.File, sqlite3vfs.OpenFlag, error) {
	v.logger.Debug("Open", "name", name, "flags", flags)

	if _, err := v.kc.Discovery().ServerVersionWithContext(v.ctx); err != nil {
		v.logger.Error("Open: server version", "err", err)
		return nil, flags, sqlite3vfs.IOError
	}

	var lastErr error
	for range v.retries {
		// Check if namespace and lockfile already exist.
		// If they don't, create them
		// if this fails, return readonlyfs

		f := NewFile(name, v)

		// Now check for lock file
		_, err := v.kc.CoreV1().ConfigMaps(v.namespace).Get(v.ctx, f.LockFileName(), metav1.GetOptions{})
		switch {
		case kerrors.IsNotFound(err):
			if err := f.setLock(sqlite3vfs.LockNone); err != nil {
				v.logger.Error("Open: set initial lock state", "err", err)
				lastErr = err
				continue
			}
		case err != nil:
			return f, flags, err
		default:
			// Every opened file starts at NONE locally. The ConfigMap's
			// stored level may be another process's lock, and adopting it
			// made this file believe it held that lock: its Lock calls
			// became no-ops and its Unlock wrote the level down under the
			// other process. This object records only the transitions it
			// performs itself, each one through the ConfigMap.
			f.lockLevel = sqlite3vfs.LockNone
			f.lockKnown = true
		}

		cms, err := f.listSectorConfigMaps()
		if err != nil {
			// Without the LIST we cannot tell a fresh file from an existing
			// one: treating it as fresh would write an empty sector 0 over
			// the first sector of an existing database. Retry, and fail if
			// the attempts run out.
			v.logger.Error("Open: list sector configmaps", "err", err)
			lastErr = err
			continue
		}
		names := make([]string, 0, len(cms))
		for _, cm := range cms {
			names = append(names, cm.Name)
		}

		v.logger.Debug("Checked for existing sectors", "sectors", names, "len(cms)", len(cms))

		if len(cms) == 0 {
			// Fresh (or externally emptied) file: create the empty sector 0
			// that the size computation and reads expect. Known to be new,
			// so this is one CREATE, with the usual conflict fallback.
			if err := f.writeSectorOnce(0, nil, false); err != nil {
				v.logger.Error("Open: create initial empty sector", "err", err)
				lastErr = err
				continue
			}
		}
		v.logger.Debug("Opened file successfully", "name", name, "flags", flags)

		return f, flags, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no open attempt was made (retries must be positive)")
	}
	v.logger.Debug("Failed to open file")

	return nil, flags, fmt.Errorf("open %q: file metadata did not settle after %d attempts: %w", name, v.retries, lastErr)
}

// Delete implements sqlite3vfs.VFS: it removes every sector ConfigMap of
// the named file and then its lock ConfigMap, retrying transient races up
// to the VFS's retry budget.
func (v *vfs) Delete(name string, dirSync bool) error {
	v.logger.Debug("Delete", "name", name, "dirSync", dirSync)
	// in case we're racing another client
	f := NewFile(name, v)
	var lastErr error
	for range f.vfs.retries + 1 {
		v.logger.Debug("Deleting configmaps representing this filename", "name", name)
		cms, err := f.listSectorConfigMaps()
		if err != nil {
			// Without the LIST the sectors cannot be found; deleting only
			// the lockfile would leave them orphaned on the cluster.
			v.logger.Error("Delete: list sector configmaps", "err", err)
			lastErr = err
			continue
		}

		aDeleteFailed := false
		for _, c := range cms {
			if err := v.kc.CoreV1().ConfigMaps(v.namespace).Delete(v.ctx, c.Name, metav1.DeleteOptions{}); err != nil {
				v.logger.Error("Delete: delete sector configmap", "configmap", c.Name, "err", err)
				aDeleteFailed = true
				continue
			}
			v.logger.Debug("Deleted configmap", "configmap", c.Name)
		}
		if aDeleteFailed {
			continue
		}

		v.logger.Debug("Deleting lockfile for this filename", "name", name)
		err = v.kc.CoreV1().ConfigMaps(v.namespace).Delete(v.ctx, f.LockFileName(), metav1.DeleteOptions{})
		if err == nil || kerrors.IsNotFound(err) {
			return nil
		}
		v.logger.Error("Delete: delete lockfile", "err", err)
		lastErr = err
	}
	v.logger.Error("Failed to delete file", "filename", name, "dirSync", dirSync, "err", lastErr)
	return sqlite3vfs.IOError
}

// Access implements sqlite3vfs.VFS. It reports every file as accessible,
// except sqlite's side files, which never exist in the cluster: sqlite
// fails with "unable to open database file: invalid argument" when it tries
// to read them while they are empty.
func (v *vfs) Access(name string, flags sqlite3vfs.AccessFlag) (bool, error) {
	v.logger.Debug("Access", "name", name, "flags", flags)
	if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-journal") {
		return false, nil
	}
	return true, nil
}

// FullPathname implements sqlite3vfs.VFS: it returns name unchanged, as
// file names are already cluster-wide (the TODO of fulfilling
// canonicalisation properly stands).
func (v *vfs) FullPathname(name string) string {
	return name
}

// CheckReservedLock implements sqlite3vfs.File. When this process itself
// holds a lock the answer is known locally and costs no API call; otherwise
// another process may hold one, so the lock ConfigMap is consulted as
// before.
func (f *file) CheckReservedLock() (bool, error) {
	if f.lockKnown && f.lockLevel > sqlite3vfs.LockNone {
		// we hold a lock
		return true, nil
	}

	currentLock, err := f.getCurrentLock()
	if err != nil {
		f.vfs.logger.Error("CheckReservedLock", "err", err)
		return false, err
	}
	if currentLock > sqlite3vfs.LockNone {
		// we hold a lock
		return true, nil
	}
	return false, nil
}
