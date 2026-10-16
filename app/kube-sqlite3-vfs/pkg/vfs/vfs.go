package vfs

import (
	"context"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/psanford/sqlite3vfs"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

const (
	LockFileNameSuffix = "lockfile"
	SectorSize         = 64 * 1024 // Max pagesize supported by SQLITE3
)

// Only var because this can't be a const
var (
	CommonSectorLabel = map[string]string{"data": "sector"}
	LockfileLabel     = map[string]string{"data": "lockfile"}
)

type vfs struct {
	kc        kubernetes.Interface
	logger    *zap.SugaredLogger
	retries   int
	namespace string
}

func NewVFS(kc kubernetes.Interface, namespace string, logger *zap.SugaredLogger, retries int) *vfs {
	return &vfs{kc: kc, logger: logger, retries: retries, namespace: namespace}
}

func (f *file) b32ByteFromString(s string) []byte {
	sb := []byte(s)
	dst := make([]byte, f.encoding.EncodedLen(len(sb)))
	f.encoding.Encode(dst, sb)
	return dst
}

func (f *file) Close() error {

	err := f.setLock(sqlite3vfs.LockNone)

	return err
}

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
		if err != nil && err != errSectorNotFound {
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
	for sectToDelete := lastRemainingSector + 1; sectToDelete <= lastSector; sectToDelete += 1 {
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
	f.vfs.logger.Debugw("FileSize", "f", f)
	if f.valid {
		return f.size, nil
	}
	cms, err := f.listSectorConfigMaps()
	if err != nil {
		f.vfs.logger.Error(err)

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
	f.vfs.logger.Debugw("FileSize", "f", f, "size", size)

	return size, nil
}

// A file object backs one sqlite connection; sqlite serialises the calls
// made per connection, so (as before this change) no extra synchronisation
// is added around the fields below.
type file struct {
	RawName      string
	vfs          *vfs
	encoding     *base32.Encoding
	SectorLabels map[string]string

	// Local view of the shared state, kept to cut Kubernetes API round
	// trips. Everything cached here is trusted only while a lock of SHARED
	// or better is held: another process may hold and modify the database
	// between our locks, so the caches are dropped the moment the lock
	// drops below SHARED (adoptLockState) and revalidated with one LIST by
	// the first FileSize after SHARED is re-acquired.
	//
	// lockLevel/lockKnown: the lock level this process believes it holds.
	// sqlite asks Lock, Unlock and CheckReservedLock around every
	// operation; the imported code GET the lock ConfigMap before each one.
	// Until lockKnown is set (a file object created by NewFile rather than
	// opened through the VFS) the ConfigMap is consulted as before.
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

// this needs to return Eof if a read is attempted off the end of the file...
func (f *file) ReadAt(p []byte, off int64) (int, error) {
	f.vfs.logger.Debugw("ReadAt", "f", f, "off", off, "len(buffer)", len(p))

	firstSector := f.sectorForPos(off)

	fileSize, err := f.FileSize()
	if err != nil {
		f.vfs.logger.Debugw("ReadAt", "off", off, "len(buffer)", len(p), "fileSize", fileSize, "err", err)

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
		// err = sqlite3vfs.IOErrorShortRead
		err = io.EOF
		f.vfs.logger.Debugw("ReadAt", "off", off, "len(buffer)", len(p), "fileSize", fileSize, "err", err)
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
	f.vfs.logger.Debugw("ReadAt", "off", off, "len(buffer)", len(p), "n", n)

	if n < len(p) {
		f.vfs.logger.Error("Read too little")
		return n, io.EOF
	}

	f.vfs.logger.Debugw("ReadAt read all expected bytes")

	return n, nil
}

func (f *file) WriteAt(p []byte, off int64) (int, error) {
	f.vfs.logger.Debugw("WriteAt", "len(p)", len(p), "off", off)

	firstSector := f.sectorForPos(off)

	lastByte := off + int64(len(p)) - 1

	lastSector := f.sectorForPos(lastByte)

	if len(p) == 0 {
		return 0, nil
	}

	var (
		nW int
	)
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
		startByteForThisSector := ((sectorIndex) * SectorSize)

		currentOffset := off + int64(nW)
		var sectorData []byte
		if lastByte > lastPossibleByteForThisSector {
			sectorData = make([]byte, SectorSize)
		} else {
			// If there's existing data, ensure the buffer is large enough to hold it
			newlyRequiredSize := (1 + lastByte) - startByteForThisSector
			if l := int64(len(data)); l > newlyRequiredSize {
				newlyRequiredSize = l
			}
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
			f.vfs.logger.Error(err)
			return nW, err
		}
		if f.valid {
			f.cacheSector(sectorIndex, sectorData)
			if end := sectorIndex*f.SectorSize() + int64(len(sectorData)); end > f.size {
				f.size = end
			}
		}
		nW += nn
	}

	return nW, nil
}

// Sync noops as we're doing the writes directly
func (f *file) Sync(flag sqlite3vfs.SyncType) error {
	f.vfs.logger.Debugw("Sync", "flag", flag)

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
	f.vfs.logger.Debugw("getCurrentLock")

	cm, err := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Get(context.TODO(), f.LockFileName(), metav1.GetOptions{})
	if err != nil {
		f.vfs.logger.Error(err)
		return sqlite3vfs.LockNone, err
	}
	lockToReturn, ok := lockLevelFromString(cm.Data["lock"])
	if !ok {
		errStr := fmt.Sprintf("lock type unknown: %v, %v", f, cm.Data["lock"])
		f.vfs.logger.Error(errStr)
		return sqlite3vfs.LockNone, errors.New(errStr)
	}
	f.vfs.logger.Debugw("getCurrentLock", "found current lock", lockToReturn)

	return lockToReturn, nil
}

// currentLockLevel returns this file's local view of the lock level,
// consulting (and adopting) the lock ConfigMap only when nothing is known
// locally yet — a file object created with NewFile rather than opened
// through the VFS. Once the state is known it is trusted: this process
// performed the transitions itself, and re-GETting the ConfigMap before
// every Lock/Unlock — as the imported code did — is one of the
// per-operation API calls this optimisation removes.
func (f *file) currentLockLevel() (sqlite3vfs.LockType, error) {
	f.vfs.logger.Debugw("currentLockLevel")
	if f.lockKnown {
		return f.lockLevel, nil
	}
	level, err := f.getCurrentLock()
	if err != nil {
		return sqlite3vfs.LockNone, err
	}
	f.lockLevel = level
	f.lockKnown = true
	return level, nil
}

func (f *file) LockFileName() string {
	localLockFileName := fmt.Sprintf("%s-%s", f.b32ByteFromString(f.RawName), LockFileNameSuffix)
	return localLockFileName
}

func (f *file) generateSectorsLabels() {
	fileNameLabel := string(f.b32ByteFromString(f.RawName))

	f.SectorLabels = make(map[string]string)
	for k, v := range CommonSectorLabel {
		f.SectorLabels[k] = v
	}

	f.SectorLabels["relevant-file"] = fileNameLabel
}

// possibly add the validation?
func (f *file) setLock(lock sqlite3vfs.LockType) error {
	f.vfs.logger.Debugw("setLock", "lock", lock)

	LockfileLabels := make(map[string]string)
	for k, v := range LockfileLabel {
		LockfileLabels[k] = v
	}
	fileNameLabel := string(f.b32ByteFromString(f.RawName))

	LockfileLabels["relevant-file"] = fileNameLabel

	lf := &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: f.LockFileName(), Labels: LockfileLabels}, Data: map[string]string{"lock": lock.String(), "relevant-file": string(f.b32ByteFromString(f.RawName))}}

	_, err := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Update(context.TODO(), lf, metav1.UpdateOptions{})
	f.vfs.logger.Debugw("setLock", "lock", lock, "err", err)
	if kerrors.IsNotFound(err) {
		lf := &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: f.LockFileName(), Labels: LockfileLabels}, Data: map[string]string{"lock": lock.String(), "relevant-file": string(f.b32ByteFromString(f.RawName))}}
		_, err := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Create(context.TODO(), lf, metav1.CreateOptions{})
		f.vfs.logger.Debugw("setLock has been created", "lock", lock, "err", err)
		if err == nil {
			f.adoptLockState(lock)
		}
		return err
	}
	if err == nil {
		f.adoptLockState(lock)
	}

	return err

}

// adoptLockState records a lock transition this process just made and drops
// the sector/size caches when the lock falls below SHARED: from that moment
// another process may modify the database, so everything cached under the
// lock is stale until the next FileSize revalidates it after SHARED is
// re-acquired.
func (f *file) adoptLockState(lock sqlite3vfs.LockType) {
	f.lockLevel = lock
	f.lockKnown = true
	if lock < sqlite3vfs.LockShared {
		f.valid = false
		f.sectors = nil
	}
}

func (f *file) Lock(elock sqlite3vfs.LockType) error {
	f.vfs.logger.Debugw("Lock", "elock", elock)
	currentLock, err := f.currentLockLevel()
	if err != nil {
		f.vfs.logger.Error(err)
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
	//  (2) SQLite never explicitly requests a pendig lock.
	if elock == sqlite3vfs.LockPending {
		return errors.New("invalid Lock() request for state pending")
	}
	//  (3) A shared lock is always held when a reserve lock is requested.
	if elock == sqlite3vfs.LockReserved && currentLock != sqlite3vfs.LockShared {
		return errors.New("can only transition to Reserved lock from Shared lock")
	}

	return f.setLock(elock)

}

func (f *file) Unlock(elock sqlite3vfs.LockType) error {
	f.vfs.logger.Debugw("Unlock", "elock", elock)

	currentLock, err := f.currentLockLevel()
	if err != nil {
		f.vfs.logger.Error(err)
		return err
	}

	if elock > sqlite3vfs.LockShared {
		f.vfs.logger.Panicf("Invalid unlock request to level %s", elock)
	}

	if currentLock < elock {
		f.vfs.logger.Panic("Cannot unlock to a level > current lock level")
	}

	if elock == currentLock {
		return nil
	}

	if elock == sqlite3vfs.LockShared {
		return f.setLock(sqlite3vfs.LockShared)
	}

	return f.setLock(sqlite3vfs.LockNone)
}

func (f *file) SectorSize() int64 {
	// 64k as we're considering each sector
	return SectorSize
}

// DeviceCharacteristics
// We'll target 64K per configmap
func (f *file) DeviceCharacteristics() sqlite3vfs.DeviceCharacteristic {
	return sqlite3vfs.IocapAtomic64K
}

func NewFile(name string, v *vfs) *file {
	o := base32.NewEncoding("abcdefghijklmnopqrstuv0123456789")
	e := o.WithPadding('x')
	f := &file{RawName: name, vfs: v, encoding: e}
	f.generateSectorsLabels()
	return f
}

// TODO, locking so other connections refused?
func (v *vfs) Open(name string, flags sqlite3vfs.OpenFlag) (sqlite3vfs.File, sqlite3vfs.OpenFlag, error) {
	v.logger.Debugw("Open", "name", name, "flags", flags)

	_, err := v.kc.Discovery().ServerVersion()
	if err != nil {
		v.logger.Error(err)
		return nil, flags, sqlite3vfs.IOError
	}

	for i := 0; i < v.retries; i++ {
		// Check if namespace and lockfile already exist.
		// If they don't, create them
		// if this fails, return readonlyfs

		f := NewFile(name, v)

		// Now check for lock file
		lf, err := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Get(context.TODO(), f.LockFileName(), metav1.GetOptions{})
		if kerrors.IsNotFound(err) {
			err = f.setLock(sqlite3vfs.LockNone)
			if err != nil {
				f.vfs.logger.Error(err)
				continue
			}
		} else if err != nil {
			return f, flags, err
		} else if level, ok := lockLevelFromString(lf.Data["lock"]); ok {
			// Adopt the stored lock level as this file object's local view:
			// it is exactly what the imported code's first getCurrentLock
			// GET would have reported.
			f.lockLevel = level
			f.lockKnown = true
		}

		cms, err := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labels.SelectorFromSet(f.SectorLabels).String()})
		names := []string{}
		for _, n := range cms.Items {
			names = append(names, n.Name)
		}

		v.logger.Debugw("Checked for existing sectors", "sectors", names, "len(cms.Items)", len(cms.Items), "err", err)
		if err != nil {
			v.logger.Debugw("err response for data configmaps", "error", err)
		}
		if len(cms.Items) == 0 {
			// Fresh (or externally emptied) file: create the empty sector 0
			// that the size computation and reads expect. Known to be new,
			// so this is one CREATE, with the usual conflict fallback.
			err := f.writeSectorOnce(0, nil, false)
			v.logger.Debugw("wrote an empty sector", "error", err)

			if err != nil {
				v.logger.Error(err)
				return f, flags, err

			}

		}
		v.logger.Debugw("Opened file successfully", "name", name, "flags", flags)

		return f, flags, nil

	}
	v.logger.Debugw("Failed to open file")
	return nil, flags, errors.New("failed to get/create file metadata too many times due to races")

}

func (v *vfs) Delete(name string, dirSync bool) error {
	v.logger.Debugw("Delete", "name", name, "dirSync", dirSync)
	// in case we're racing another client
	f := NewFile(name, v)
	for i := 0; i <= f.vfs.retries; i++ {

		v.logger.Debugw("Deleting configmaps representing this filename", "name", name)
		cms, err := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labels.SelectorFromSet(f.SectorLabels).String()})
		if err != nil {
			v.logger.Errorw("Delete's get cms failed", "err", err)
		}
		v.logger.Debugw("Delete get cms", "cms", cms, "err", err)
		aDeleteFailed := false

		for _, c := range cms.Items {
			err := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Delete(context.TODO(), c.Name, metav1.DeleteOptions{})
			if err != nil {
				v.logger.Errorw("Delete failed to delete cm", "cm", c, "err", err)
				aDeleteFailed = true
				continue
			}
			v.logger.Debugw("Deleted configmap", "configmap", c)

		}
		if aDeleteFailed {
			continue
		}

		v.logger.Debugw("Deleting lockfile for this filename", "name", name)
		err = f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Delete(context.TODO(), f.LockFileName(), metav1.DeleteOptions{})
		if kerrors.IsNotFound(err) || err == nil {
			return nil
		} else {
			f.vfs.logger.Error(err)
			continue
		}
	}
	f.vfs.logger.Errorw("Failed to delete file", "filename", name, "dirSync", dirSync)
	return sqlite3vfs.IOError
}

// Access tests for access permission. Returns true if the requested permission is available.
func (v *vfs) Access(name string, flags sqlite3vfs.AccessFlag) (bool, error) {
	v.logger.Debugw("Access", "name", name, "flags", flags)
	// Required because SQLITE3 fails with "unable to open database file: invalid argument" when it tries to read this when they're empty, and we return io.EOF
	if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-journal") {
		return false, nil
	}
	return true, nil
}

// FullPathname returns the canonicalized version of name.
// TODO actually fulfil this
func (v *vfs) FullPathname(name string) string {
	return name
}

// CheckReservedLock reports whether a lock is held. When this process
// itself holds one the answer is known locally and costs no API call;
// otherwise another process may hold one, so the lock ConfigMap is
// consulted as before.
func (f *file) CheckReservedLock() (bool, error) {

	if f.lockKnown && f.lockLevel > sqlite3vfs.LockNone {
		// we hold a lock
		return true, nil
	}

	currentLock, err := f.getCurrentLock()
	if err != nil {
		f.vfs.logger.Error(err)
		return false, err
	}
	if currentLock > sqlite3vfs.LockNone {
		// we hold a lock
		return true, nil
	}
	return false, nil
}
