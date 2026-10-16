package vfs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/psanford/sqlite3vfs"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

var errSectorNotFound = errors.New("sector not found")

// maxCachedSectors bounds the per-file sector cache: 256 * 64 KiB = 16 MiB
// per open file. When the cap is hit the cache is dropped wholesale rather
// than evicted entry by entry: crude, but bounded, and correctness never
// depends on the cache — only the API-call count does.
const maxCachedSectors = 256

type Sector struct {
	Index  int64
	Data   []byte
	Labels map[string]string
}

func (f *file) sectorForPos(pos int64) int64 {

	s := (pos / SectorSize)
	f.vfs.logger.Debugw("sectorForPos", "pos", pos, "sector", s)

	return s
}

func (f *file) deleteSector(sectorIndex int64) error {
	n := f.sectorNameFromSectorIndex(sectorIndex)
	err := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Delete(context.TODO(), n, metav1.DeleteOptions{})
	f.vfs.logger.Debugw("deleteSector", "sectorIndex", sectorIndex, "err", err)

	return err
}

// writeSectorOnce writes a sector's data with exactly one API call in the
// common path, choosing UPDATE or CREATE from what is already known about
// the sector's existence:
//
//   - known to exist: UPDATE, falling back to CREATE only if some other
//     client deleted the sector behind our back;
//   - known to be new: CREATE, falling back to UPDATE only if a concurrent
//     writer created the same sector first.
//
// The code as imported always tried CREATE first and fell back to UPDATE,
// so overwriting an existing sector — the common case for SQLite, which
// rewrites pages in place — cost two API calls every time.
func (f *file) writeSectorOnce(index int64, data []byte, exists bool) error {
	f.vfs.logger.Debugw("writeSectorOnce", "sectorIndex", index, "exists", exists)
	sectorName := f.sectorNameFromSectorIndex(index)
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sectorName,
			Namespace: f.vfs.namespace,
			Labels:    f.SectorLabels,
		},
		BinaryData: map[string][]byte{"sector": data},
		Data:       map[string]string{"filename": f.RawName},
	}
	var err error
	if exists {
		_, err = f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Update(context.TODO(), cm, metav1.UpdateOptions{})
		if kerrors.IsNotFound(err) {
			// The sector vanished under us (another client deleted it);
			// recreate it rather than silently dropping the write.
			_, err = f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Create(context.TODO(), cm, metav1.CreateOptions{})
		}
	} else {
		_, err = f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Create(context.TODO(), cm, metav1.CreateOptions{})
		if kerrors.IsAlreadyExists(err) {
			// The sector appeared under us (a concurrent writer created
			// it first); overwrite it rather than failing the write.
			_, err = f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Update(context.TODO(), cm, metav1.UpdateOptions{})
		}
	}
	if err != nil {
		f.vfs.logger.Error(err)
	}
	return err
}

// WriteSector writes one sector, known to be new (CREATE, with the UPDATE
// conflict fallback). Used by Open for the empty first sector and by
// cmd/db-upload, which streams fresh sectors.
func (f *file) WriteSector(s *Sector) error {
	return f.writeSectorOnce(s.Index, s.Data, false)
}

func (f *file) sectorNameFromSectorIndex(sectorIndex int64) string {

	sectorName := fmt.Sprintf("%s-%d", f.b32ByteFromString(f.RawName), sectorIndex)
	f.vfs.logger.Debugw("sectorNameFromSectorIndex", "sectorIndex", sectorIndex, "sectorName", sectorName)

	return sectorName
}

// sectorIndexFromConfigMapName parses the sector index back out of a sector
// ConfigMap name ("<base32 filename>-<index>"). The base32 alphabet of
// b32ByteFromString contains no '-', so the last '-' always starts the index.
func (f *file) sectorIndexFromConfigMapName(name string) (int64, bool) {
	i := strings.LastIndex(name, "-")
	if i < 0 {
		return 0, false
	}
	idx, err := strconv.ParseInt(name[i+1:], 10, 64)
	if err != nil {
		return 0, false
	}
	return idx, true
}

// getSector returns a sector's bytes, serving them from the cache while the
// cache is valid (i.e. while a lock of SHARED or better is held).
//
// Unlike the code as imported, it NEVER creates a missing sector: the old
// getSector wrote an empty ConfigMap on read and then dereferenced the nil
// response it had just replaced, panicking on any write that reached an
// unwritten sector (which capped every file at one 64 KiB sector). A missing
// sector is now reported as errSectorNotFound and the caller decides what
// that means: reads treat it as empty bytes, writes treat it as a sector to
// CREATE.
func (f *file) getSector(sectorIndex int64) (*Sector, error) {
	f.vfs.logger.Debugw("getSector", "sectorIndex", sectorIndex)
	data, err := f.sectorData(sectorIndex)
	if err != nil {
		return nil, err
	}
	return &Sector{Index: sectorIndex, Data: data}, nil
}

// sectorData fetches one sector's bytes: from the cache when valid,
// otherwise with one GET. A missing sector reports errSectorNotFound and
// nothing is ever written to the cluster on a read.
func (f *file) sectorData(sectorIndex int64) ([]byte, error) {
	if f.valid {
		if data, ok := f.sectors[sectorIndex]; ok {
			return data, nil
		}
	}
	sectorName := f.sectorNameFromSectorIndex(sectorIndex)
	cm, err := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Get(context.TODO(), sectorName, metav1.GetOptions{})
	f.vfs.logger.Debugw("sectorData", "sectorIndex", sectorIndex, "err", err)

	if kerrors.IsNotFound(err) {
		return nil, errSectorNotFound
	} else if err != nil {
		f.vfs.logger.Error(err)
		return nil, sqlite3vfs.IOErrorShortRead
	}

	data := make([]byte, len(cm.BinaryData["sector"]))
	copy(data, cm.BinaryData["sector"])
	if f.valid {
		f.cacheSector(sectorIndex, data)
	}
	return data, nil
}

// sectorDataForWrite fetches a sector's current bytes for an overwrite,
// reporting whether the sector exists. While the cache is valid a hit
// answers with no API call at all; otherwise it is one GET. A missing
// sector is reported as (nil, false, nil) so the write can CREATE it.
func (f *file) sectorDataForWrite(sectorIndex int64) (data []byte, exists bool, err error) {
	f.vfs.logger.Debugw("sectorDataForWrite", "sectorIndex", sectorIndex)
	if f.valid {
		if data, ok := f.sectors[sectorIndex]; ok {
			return data, true, nil
		}
	}
	sectorName := f.sectorNameFromSectorIndex(sectorIndex)
	cm, err := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Get(context.TODO(), sectorName, metav1.GetOptions{})
	if err == nil {
		data = make([]byte, len(cm.BinaryData["sector"]))
		copy(data, cm.BinaryData["sector"])
		if f.valid {
			f.cacheSector(sectorIndex, data)
		}
		return data, true, nil
	}
	if kerrors.IsNotFound(err) {
		return nil, false, nil
	}
	f.vfs.logger.Error(err)
	return nil, false, err
}

// cacheSector stores a sector in the cache, dropping the whole cache when
// the maxCachedSectors cap is exceeded.
func (f *file) cacheSector(index int64, data []byte) {
	if f.sectors == nil {
		f.sectors = make(map[int64][]byte)
	}
	if _, cached := f.sectors[index]; !cached && len(f.sectors) >= maxCachedSectors {
		f.sectors = make(map[int64][]byte)
	}
	f.sectors[index] = data
}

// listSectorConfigMaps lists this file's sector ConfigMaps in one API call.
// A LIST response carries each ConfigMap's BinaryData, so the file size is
// computable from it alone, without the follow-up GET of the last sector
// the imported FileSize made.
func (f *file) listSectorConfigMaps() ([]corev1.ConfigMap, error) {
	cms, err := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labels.SelectorFromSet(f.SectorLabels).String()})
	f.vfs.logger.Debugw("listSectorConfigMaps", "f.RawName", f.RawName, "len(configmaps)", len(cms.Items), "err", err)
	return cms.Items, err
}

// sizeFromSectorList computes the file size from a sector LIST: the largest
// index*SectorSize+len(data) over all sectors. This assumes sectors are
// dense up to the last one, which holds for anything SQLite writes (and for
// everything the old code could write at all); holes left by out-of-order
// page writes are tolerated, they only make the size conservative.
func (f *file) sizeFromSectorList(cms []corev1.ConfigMap) int64 {
	var size int64
	for i := range cms {
		idx, ok := f.sectorIndexFromConfigMapName(cms[i].Name)
		if !ok {
			f.vfs.logger.Errorw("sector configmap with unparsable name", "name", cms[i].Name)
			continue
		}
		if s := idx*f.SectorSize() + int64(len(cms[i].BinaryData["sector"])); s > size {
			size = s
		}
	}
	return size
}

// getSectorRange fetches sectors [firstSector, lastSector]. A sector that
// does not exist contributes no bytes (a read into a hole yields short data
// and io.EOF at the ReadAt layer) — nothing is created on a read.
func (f *file) getSectorRange(firstSector, lastSector int64) ([]*Sector, error) {
	f.vfs.logger.Debugw("getSectorRange", "firstSector", firstSector, "lastSector", lastSector)

	sectors := make([]*Sector, ((lastSector - firstSector) + 1)) // +1 because 0 indexes

	for i := firstSector; i <= lastSector; i++ {
		data, err := f.sectorData(i)
		if err != nil && err != errSectorNotFound {
			f.vfs.logger.Error(err)
			return nil, sqlite3vfs.IOErrorRead
		}
		sectors[i-firstSector] = &Sector{Index: i, Data: data}
	}

	return sectors, nil
}
