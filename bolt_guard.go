package main

// Crash-safety guard for opening the bbolt usage store.
//
// bbolt memory-maps the database file and dereferences page pointers without
// bounds checks. When a crash truncates the file between write and fsync, the
// committed meta page can reference pages beyond EOF; the first dereference
// then raises SIGBUS, which is fatal and unrecoverable in Go (no recover(),
// no error return — the process simply dies, on every restart, until an
// operator repairs the file by hand).
//
// validateBoltFileBounds re-reads the two on-disk meta pages (mirroring the
// pinned bbolt v1.3.8 layout) and proves that every page the primary meta may
// touch — the page count itself and the freelist page with its overflow chain
// and id count — lies inside the file, BEFORE any mmap exists. The common
// truncation class is thereby converted into a clean, actionable error.
//
// Limits, stated precisely:
//   - truncation that removes at least one page counted by the active meta:
//     rejected up front;
//   - torn header / random garbage: rejected by the meta checksum, or by
//     bbolt's own validation, as a normal error;
//   - adversarially crafted corruption that keeps every bound consistent but
//     scrambles page contents can still make bbolt panic (converted to an
//     error by the recover in newUsageStore) or, in the worst case, fault the
//     mmap: no in-process code can promise otherwise. Operators recover with
//     the paired passport backup.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
)

const (
	boltMagic       = uint32(0xED0CDAED)
	boltVersion     = uint32(2)
	boltMinPageSize = uint32(512)
	boltMaxPageSize = uint32(1 << 16)
	// boltPageHeaderSize matches bbolt's page struct (id, flags, count,
	// overflow) and boltMetaSize matches unsafe.Sizeof(meta{}) minus nothing:
	// the checksum closes the struct.
	boltPageHeaderSize = 16
	boltMetaSize       = 64
)

// ErrBoltCorrupt is returned (wrapped) when the usage store file fails the
// pre-mmap structural check. Callers should surface the remediation hint.
var ErrBoltCorrupt = errors.New("usage store corrupt")

func boltCorruptError(path, detail string) error {
	return fmt.Errorf("%w: %s: %s; restore it from a passport backup (see -restore-manifest) or delete the file to start a fresh store", ErrBoltCorrupt, path, detail)
}

// boltMetaView is the decoded subset of bbolt's on-disk meta page needed for
// the bounds proof.
type boltMetaView struct {
	pageSize  uint32
	freelist  uint64
	pgid      uint64
	txid      uint64
	freelistC uint16
	overflow  uint32
}

// validateBoltFileBounds returns nil for nonexistent or empty files (fresh
// stores) and for files whose active meta page only references in-file pages.
func validateBoltFileBounds(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}

	// The page size lives inside the meta page, so probe the head of the file
	// (two maximum-size pages covers meta1 on any host page size) and decode
	// from there.
	head := make([]byte, 2*boltMaxPageSize)
	n, err := file.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	head = head[:n]

	pageSize := binary.LittleEndian.Uint32(head[boltPageHeaderSize+8 : boltPageHeaderSize+12])
	if pageSize < boltMinPageSize || pageSize > boltMaxPageSize || pageSize&(pageSize-1) != 0 {
		return boltCorruptError(path, fmt.Sprintf("invalid page size %d", pageSize))
	}

	var chosen *boltMetaView
	for page := uint64(0); page < 2; page++ {
		view, ok := decodeBoltMeta(head, page, pageSize)
		if !ok {
			// bbolt skips metas that fail magic/version/checksum too.
			continue
		}
		if chosen == nil || view.txid > chosen.txid {
			view := view
			chosen = &view
		}
	}
	if chosen == nil {
		// No self-consistent meta: bbolt.Open reports a clean error itself.
		return nil
	}
	if chosen.pageSize != pageSize {
		pageSize = chosen.pageSize
	}

	if chosen.pgid == 0 || uint64(chosen.pgid)*uint64(pageSize) > uint64(info.Size()) {
		return boltCorruptError(path, fmt.Sprintf("meta page count %d exceeds file size %d", chosen.pgid, info.Size()))
	}
	if chosen.freelist == 0 {
		return nil
	}
	if chosen.freelist >= chosen.pgid {
		return boltCorruptError(path, fmt.Sprintf("freelist page %d outside page count %d", chosen.freelist, chosen.pgid))
	}
	header, ok := readBoltPageHeader(file, chosen.freelist, pageSize)
	if !ok {
		return boltCorruptError(path, fmt.Sprintf("freelist page %d beyond file size %d", chosen.freelist, info.Size()))
	}
	overflow := binary.LittleEndian.Uint32(header[12:16])
	count := binary.LittleEndian.Uint16(header[4:6])
	pages := uint64(overflow) + 1
	if (chosen.freelist+pages)*uint64(pageSize) > uint64(info.Size()) {
		return boltCorruptError(path, fmt.Sprintf("freelist page %d (overflow %d) exceeds file size %d", chosen.freelist, overflow, info.Size()))
	}
	// freelist.read copies count ids (with one slot skipped when count is the
	// 0xFFFF overflow sentinel); both must fit inside the page chain.
	maxIDs := (pages*uint64(pageSize) - boltPageHeaderSize) / 8
	ids := uint64(count)
	if ids == 0xFFFF {
		ids = readBoltFreelistCount(file, chosen.freelist, pageSize)
		maxIDs--
	}
	if ids > maxIDs {
		return boltCorruptError(path, fmt.Sprintf("freelist id count %d exceeds page capacity %d", ids, maxIDs))
	}
	return nil
}

// decodeBoltMeta parses and checksum-verifies the meta page at the given page
// index. It mirrors bbolt's validate(): magic, version, FNV-64a checksum.
func decodeBoltMeta(head []byte, page uint64, pageSize uint32) (boltMetaView, bool) {
	start := int(page)*int(pageSize) + boltPageHeaderSize
	if start+boltMetaSize > len(head) {
		return boltMetaView{}, false
	}
	meta := head[start : start+boltMetaSize]
	if binary.LittleEndian.Uint32(meta[0:4]) != boltMagic || binary.LittleEndian.Uint32(meta[4:8]) != boltVersion {
		return boltMetaView{}, false
	}
	sum := fnv.New64a()
	_, _ = sum.Write(meta[:56])
	if sum.Sum64() != binary.LittleEndian.Uint64(meta[56:64]) {
		return boltMetaView{}, false
	}
	return boltMetaView{
		pageSize: binary.LittleEndian.Uint32(meta[8:12]),
		freelist: binary.LittleEndian.Uint64(meta[32:40]),
		pgid:     binary.LittleEndian.Uint64(meta[40:48]),
		txid:     binary.LittleEndian.Uint64(meta[48:56]),
	}, true
}

// readBoltPageHeader reads the 16-byte page header at the given page index.
func readBoltPageHeader(file *os.File, page uint64, pageSize uint32) ([16]byte, bool) {
	var header [16]byte
	_, err := file.ReadAt(header[:], int64(page)*int64(pageSize))
	return header, err == nil
}

// readBoltFreelistCount reads the leading count element used when the page's
// count field holds the 0xFFFF sentinel.
func readBoltFreelistCount(file *os.File, page uint64, pageSize uint32) uint64 {
	var raw [8]byte
	if _, err := file.ReadAt(raw[:], int64(page)*int64(pageSize)+boltPageHeaderSize); err != nil {
		return 1 << 62 // unreadable count: force rejection via the bound check
	}
	return binary.LittleEndian.Uint64(raw[:])
}
