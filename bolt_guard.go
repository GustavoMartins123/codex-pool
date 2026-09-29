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
// validateBoltFileBounds re-reads the on-disk meta pages (mirroring the
// pinned bbolt v1.3.8 layout and its page-size discovery, including the
// 1 KiB..16 MiB second-meta scan) and proves that every page the primary
// meta may touch — the page count itself and the freelist page with its
// overflow chain and id count — lies inside the file, BEFORE any mmap
// exists. The common truncation class is thereby converted into a clean,
// actionable error.
//
// Limits, stated precisely:
//   - truncation that removes at least one page counted by the active meta:
//     rejected up front;
//   - torn header / random garbage / files too small to hold a meta page:
//     rejected (or left to bbolt's own clean validation error);
//   - adversarially crafted corruption that keeps every bound consistent but
//     scrambles page contents can still make bbolt panic (converted to an
//     error by the recover in newUsageStore) or, in the worst case, fault the
//     mmap: no in-process code can promise otherwise. Operators recover with
//     the paired passport backup.
//
// Tolerance contract: the guard only REJECTS when a checksum-valid meta page
// (the kind bbolt itself will trust) references pages beyond EOF. Anything
// it cannot decode is passed through to bbolt, whose pre-mmap header
// validation reports ordinary clean errors — the guard must never be less
// tolerant than the library it mirrors.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
)

const (
	boltMagic = uint32(0xED0CDAED)
	boltVer   = uint32(2)
	// boltPageHeaderSize matches bbolt's page struct (id, flags, count,
	// overflow); a meta page is the header followed by the 64-byte meta
	// struct whose last field is the FNV-64a checksum.
	boltPageHeaderSize = 16
	boltMetaSize       = 64
	boltMinPageSize    = 512
	boltMaxPageSize    = 1024 << 14 // 16 MiB — mirrors bbolt's own scan cap
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
	pageSize uint32
	freelist uint64
	pgid     uint64
	txid     uint64
}

// validateBoltFileBounds returns nil for nonexistent or empty files (fresh
// stores) and for files whose active meta page only references in-file
// pages. It must never panic: every read is a bounded, length-checked probe.
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
	size := info.Size()
	if size == 0 {
		return nil
	}
	// A bbolt database is at least two pages; anything below the minimum
	// page size can never decode. bbolt rejects these too ("invalid
	// database"), but classifying here gives operators the remediation hint
	// instead of a bare library error.
	if size < int64(boltMinPageSize) {
		return boltCorruptError(path, fmt.Sprintf("file size %d is smaller than the minimum bolt page size %d", size, boltMinPageSize))
	}

	// Step 1 — discover the page size exactly like bbolt's getPageSize():
	// a checksum-valid meta 0 wins, otherwise meta 1 is probed at candidate
	// page sizes from 1 KiB to 16 MiB (bounded by the file size). This says
	// NOTHING about which meta page represents the database's current state.
	pageSize, ok := discoverBoltPageSize(file, size)
	if !ok {
		// No checksum-valid meta anywhere: bbolt's own header validation
		// produces a clean error; nothing to prove.
		return nil
	}

	// Step 2 — with the page size known, read BOTH meta pages and choose the
	// current one exactly like bbolt's DB.meta(): among the checksum-valid
	// metas, the HIGHEST txid wins (falling back to the older valid one).
	// Reusing the discovery winner here would miss the classic torn state:
	// meta 0 valid but old (small page count that fits the truncated file),
	// meta 1 valid and newer with a page count beyond EOF — bbolt would use
	// meta 1 and fault.
	meta0, ok0 := readBoltMetaAt(file, 0)
	meta1, ok1 := readBoltMetaAt(file, int64(pageSize))
	chosen := chooseBoltMeta(meta0, ok0, meta1, ok1)
	if chosen == nil {
		return nil
	}

	if chosen.pgid == 0 || uint64(chosen.pgid)*uint64(pageSize) > uint64(size) {
		return boltCorruptError(path, fmt.Sprintf("meta page count %d exceeds file size %d", chosen.pgid, size))
	}
	if chosen.freelist == 0 {
		return nil
	}
	if chosen.freelist >= chosen.pgid {
		return boltCorruptError(path, fmt.Sprintf("freelist page %d outside page count %d", chosen.freelist, chosen.pgid))
	}
	header, ok := readBoltPageHeader(file, chosen.freelist, pageSize)
	if !ok {
		return boltCorruptError(path, fmt.Sprintf("freelist page %d beyond file size %d", chosen.freelist, size))
	}
	overflow := binary.LittleEndian.Uint32(header[12:16])
	count := binary.LittleEndian.Uint16(header[4:6])
	pages := uint64(overflow) + 1
	if (chosen.freelist+pages)*uint64(pageSize) > uint64(size) {
		return boltCorruptError(path, fmt.Sprintf("freelist page %d (overflow %d) exceeds file size %d", chosen.freelist, overflow, size))
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

// discoverBoltPageSize mirrors bbolt's getPageSize(): the page size of a
// checksum-valid meta 0, otherwise of a checksum-valid meta 1 found at a
// candidate page size (1 KiB doubling to 16 MiB, bounded by the file size).
// The bool is false when no valid meta exists anywhere bbolt would look.
func discoverBoltPageSize(file *os.File, size int64) (uint32, bool) {
	if m, ok := readBoltMetaAt(file, 0); ok && plausibleBoltPageSize(m.pageSize) {
		return m.pageSize, true
	}
	for i := 0; i <= 14; i++ {
		pos := int64(1024) << uint(i)
		if pos >= size-1024 {
			break
		}
		if m, ok := readBoltMetaAt(file, pos); ok && plausibleBoltPageSize(m.pageSize) {
			return m.pageSize, true
		}
	}
	return 0, false
}

// chooseBoltMeta mirrors bbolt's DB.meta(): among the checksum-valid meta
// pages, the one with the highest txid represents the database's current
// state; an invalid higher-txid meta falls back to the older valid one.
func chooseBoltMeta(meta0 *boltMetaView, ok0 bool, meta1 *boltMetaView, ok1 bool) *boltMetaView {
	chosen, chosenOK := meta0, ok0
	if ok1 && (!ok0 || meta1.txid > meta0.txid) {
		chosen, chosenOK = meta1, true
	}
	if !chosenOK {
		return nil
	}
	return chosen
}

// readBoltMetaAt decodes and checksum-verifies the meta page whose page
// starts at the given byte offset. Mirrors bbolt's validate(): magic,
// version, FNV-64a checksum over the first 56 meta bytes.
func readBoltMetaAt(file *os.File, pageOffset int64) (*boltMetaView, bool) {
	raw := make([]byte, boltMetaSize)
	if _, err := file.ReadAt(raw, pageOffset+boltPageHeaderSize); err != nil {
		// io.EOF (short file) and anything else: not decodable, not our call.
		return nil, false
	}
	if binary.LittleEndian.Uint32(raw[0:4]) != boltMagic || binary.LittleEndian.Uint32(raw[4:8]) != boltVer {
		return nil, false
	}
	sum := fnv.New64a()
	_, _ = sum.Write(raw[:56])
	if sum.Sum64() != binary.LittleEndian.Uint64(raw[56:64]) {
		return nil, false
	}
	return &boltMetaView{
		pageSize: binary.LittleEndian.Uint32(raw[8:12]),
		freelist: binary.LittleEndian.Uint64(raw[32:40]),
		pgid:     binary.LittleEndian.Uint64(raw[40:48]),
		txid:     binary.LittleEndian.Uint64(raw[48:56]),
	}, true
}

func plausibleBoltPageSize(pageSize uint32) bool {
	return pageSize >= boltMinPageSize && pageSize <= boltMaxPageSize && pageSize&(pageSize-1) == 0
}

// readBoltPageHeader reads the 16-byte page header at the given page index.
// ReadAt only returns nil error when the full header is inside the file.
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
