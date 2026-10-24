package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"go.etcd.io/bbolt"
)

type historyStudyMetric struct {
	Name                string  `json:"name"`
	Samples             int     `json:"samples"`
	OperationsPerSample int     `json:"operations_per_sample"`
	P50MS               float64 `json:"p50_ms"`
	P95MS               float64 `json:"p95_ms"`
	P99MS               float64 `json:"p99_ms"`
	MeanAllocatedBytes  uint64  `json:"mean_allocated_bytes"`
}

func historyStudyMeasure(t *testing.T, name string, count int, run func() error) historyStudyMetric {
	return historyStudyMeasureBatch(t, name, count, 1, run)
}

func historyStudyMeasureBatch(t *testing.T, name string, count, batch int, run func() error) historyStudyMetric {
	t.Helper()
	if err := run(); err != nil {
		t.Fatal(err)
	}
	durations := make([]time.Duration, count)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := range count {
		start := time.Now()
		for range batch {
			if err := run(); err != nil {
				t.Fatal(err)
			}
		}
		durations[i] = time.Since(start) / time.Duration(batch)
	}
	runtime.ReadMemStats(&after)
	metric := historyStudyQuantiles(name, durations)
	metric.OperationsPerSample = batch
	metric.MeanAllocatedBytes = (after.TotalAlloc - before.TotalAlloc) / uint64(count*batch)
	return metric
}

func historyStudyQuantiles(name string, durations []time.Duration) historyStudyMetric {
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	at := func(percent int) float64 {
		index := (len(durations)*percent+99)/100 - 1
		if index < 0 {
			index = 0
		}
		return float64(durations[index]) / float64(time.Millisecond)
	}
	return historyStudyMetric{Name: name, Samples: len(durations), OperationsPerSample: 1, P50MS: at(50), P95MS: at(95), P99MS: at(99)}
}

type historyStudyCompression struct {
	Codec          string             `json:"codec"`
	BlockBytes     int                `json:"block_bytes"`
	Blocks         int                `json:"blocks"`
	ArchiveBytes   int                `json:"archive_bytes"`
	TailRawBytes   int                `json:"tail_raw_bytes"`
	PercentOfJSONL float64            `json:"percent_of_jsonl"`
	Encode         historyStudyMetric `json:"encode"`
	FullRestore    historyStudyMetric `json:"full_restore"`
	TailRestore    historyStudyMetric `json:"tail_restore"`
}

type historyStudyCorpus struct {
	Name             string                    `json:"name"`
	Events           int                       `json:"events"`
	JSONLBytes       int                       `json:"jsonl_bytes"`
	BoltBytes        int64                     `json:"bolt_bytes"`
	HTTPHistoryBytes int                       `json:"http_history_bytes"`
	HTTPHistoryPages int                       `json:"http_history_pages"`
	CurrentPage      historyStudyMetric        `json:"current_first_page"`
	CurrentFull      historyStudyMetric        `json:"current_full_history"`
	Preview          historyStudyMetric        `json:"indexed_preview"`
	Compression      []historyStudyCompression `json:"compression"`
}

type historyStudyListing struct {
	Accounts       int                `json:"accounts"`
	TotalSessions  int                `json:"total_sessions"`
	PerAccount     int                `json:"sessions_per_account"`
	CurrentVisible int                `json:"current_visible"`
	Current        historyStudyMetric `json:"current_scan"`
	Indexed        historyStudyMetric `json:"indexed_page_50"`
}

type historyStudyConcurrency struct {
	Writers         int                 `json:"writers"`
	Readers         int                 `json:"readers"`
	Writes          int                 `json:"writes"`
	EventsPerSecond float64             `json:"events_per_second"`
	WriteLatency    historyStudyMetric  `json:"write_latency"`
	ReadLatency     *historyStudyMetric `json:"read_latency,omitempty"`
}

type historyStudyDisk struct {
	BeforeBytes       int64              `json:"before_bytes"`
	AfterDeleteBytes  int64              `json:"after_delete_bytes"`
	AfterCompactBytes int64              `json:"after_compact_bytes"`
	ArchiveBytes      int64              `json:"archive_bytes"`
	Publish           historyStudyMetric `json:"publish_fsync"`
	Recall            historyStudyMetric `json:"recall_from_file"`
	Compact           historyStudyMetric `json:"bolt_compact"`
}

type historyCapacityStudy struct {
	Version     int                       `json:"version"`
	RunUTC      string                    `json:"run_utc"`
	Corpus      []historyStudyCorpus      `json:"corpus"`
	Listings    []historyStudyListing     `json:"listings"`
	Concurrency []historyStudyConcurrency `json:"concurrency"`
	Disk        historyStudyDisk          `json:"disk"`
}

func historyStudyBroker(t *testing.T) (*sessionBroker, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic.db")
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	broker, err := newSessionBroker(&PassportStore{db: db})
	if err != nil {
		t.Fatal(err)
	}
	return broker, path
}

func historyStudySeed(t *testing.T, broker *sessionBroker, id, principal string, events []sessionEvent) *sessionRecord {
	t.Helper()
	s := &sessionRecord{HistoryComplete: true, Session: runnerproto.Session{ID: id, PrincipalID: principal, ClientID: "synthetic-client", Agent: "codex", Model: "synthetic-model", State: "closed", Approvals: []runnerproto.Approval{}, Summaries: []runnerproto.TurnSummary{}}}
	if len(events) == 0 {
		if err := broker.saveLocked(s, "", principal); err != nil {
			t.Fatal(err)
		}
	}
	for start := 0; start < len(events); start += 1000 {
		batch := append([]sessionEvent{}, events[start:min(start+1000, len(events))]...)
		if err := broker.saveLocked(s, "", principal, batch...); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func historyStudyCurrentFull(broker *sessionBroker, s *sessionRecord) (int, int, error) {
	var cursor uint64
	var through *uint64
	bytes, pages := 0, 0
	for {
		page, err := broker.history(s.PrincipalID, s.ID, cursor, through)
		if err != nil {
			return 0, 0, err
		}
		wire, err := json.Marshal(page)
		if err != nil {
			return 0, 0, err
		}
		var decoded sessionHistoryPage
		if err := json.Unmarshal(wire, &decoded); err != nil {
			return 0, 0, err
		}
		bytes += len(wire)
		pages++
		cursor = decoded.Cursor
		through = &decoded.Through
		if !decoded.HasMore {
			break
		}
	}
	return bytes, pages, nil
}

func TestSessionHistoryCapacityStudy(t *testing.T) {
	output := os.Getenv("SESSION_HISTORY_STUDY_OUTPUT")
	if output == "" {
		t.Skip("set SESSION_HISTORY_STUDY_OUTPUT to run the explicit capacity study")
	}
	study := historyCapacityStudy{Version: 1, RunUTC: time.Now().UTC().Format(time.RFC3339)}
	codecs := historyStudyCodecs(t)
	for _, fixture := range []struct {
		count   int
		entropy bool
	}{{1000, false}, {10000, false}, {100000, false}, {10000, true}} {
		name := "mixed"
		if fixture.entropy {
			name = "high_entropy"
		}
		t.Logf("corpus %s/%d", name, fixture.count)
		events := historyStudyFixture(fixture.count, fixture.entropy)
		rawBytes := 0
		for _, event := range events {
			raw, _ := json.Marshal(event)
			rawBytes += len(raw) + 1
		}
		broker, path := historyStudyBroker(t)
		s := historyStudySeed(t, broker, "synthetic-session", "synthetic-principal", events)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		corpus := historyStudyCorpus{Name: name, Events: fixture.count, JSONLBytes: rawBytes, BoltBytes: info.Size()}
		corpus.CurrentPage = historyStudyMeasure(t, "current_page_256", 100, func() error {
			page, err := broker.history(s.PrincipalID, s.ID, 0, nil)
			if err != nil {
				return err
			}
			_, err = json.Marshal(page)
			return err
		})
		corpus.CurrentFull = historyStudyMeasure(t, "current_full_history", 7, func() error {
			var err error
			corpus.HTTPHistoryBytes, corpus.HTTPHistoryPages, err = historyStudyCurrentFull(broker, s)
			return err
		})
		preview, _ := json.Marshal(historyStudyPreviews(events))
		if err := broker.store.db.Update(func(tx *bbolt.Tx) error {
			bucket, err := tx.CreateBucketIfNotExists([]byte("study_previews"))
			if err != nil {
				return err
			}
			return bucket.Put([]byte(s.ID), preview)
		}); err != nil {
			t.Fatal(err)
		}
		corpus.Preview = historyStudyMeasureBatch(t, "indexed_preview_three_messages", 100, 1000, func() error {
			return broker.store.db.View(func(tx *bbolt.Tx) error {
				var preview []historyStudyPreview
				return json.Unmarshal(tx.Bucket([]byte("study_previews")).Get([]byte(s.ID)), &preview)
			})
		})
		for _, codec := range codecs {
			for _, blockBytes := range []int{256 << 10, 1 << 20} {
				var archive historyStudyManifest
				encode := historyStudyMeasure(t, "encode", 7, func() error {
					var err error
					archive, err = historyStudyBuildArchive(events, codec, blockBytes)
					return err
				})
				header, _ := json.Marshal(archive)
				size := len(header)
				for _, block := range archive.Blocks {
					size += len(block.data)
				}
				restore := historyStudyMeasure(t, "restore_all_verified", 7, func() error {
					decoded, err := historyStudyRestore(archive, archive.Principal, codec)
					if err == nil && len(decoded) != len(events) {
						return fmt.Errorf("restore count mismatch")
					}
					return err
				})
				last := archive.Blocks[len(archive.Blocks)-1]
				tail := historyStudyMeasure(t, "decode_last_block", 50, func() error { _, err := historyStudyDecodeBlock(last, last.data, codec); return err })
				corpus.Compression = append(corpus.Compression, historyStudyCompression{Codec: codec.name, BlockBytes: blockBytes, Blocks: len(archive.Blocks), ArchiveBytes: size, TailRawBytes: last.RawBytes, PercentOfJSONL: 100 * float64(size) / float64(rawBytes), Encode: encode, FullRestore: restore, TailRestore: tail})
			}
		}
		study.Corpus = append(study.Corpus, corpus)
	}
	for _, accounts := range []int{1, 10, 100} {
		t.Logf("list %d accounts", accounts)
		study.Listings = append(study.Listings, historyStudyList(t, accounts))
	}
	for _, writers := range []int{1, 10, 50} {
		for _, readers := range []int{0, 4} {
			t.Logf("concurrency %d writers/%d readers", writers, readers)
			study.Concurrency = append(study.Concurrency, historyStudyConcurrent(t, writers, readers))
		}
	}
	study.Disk = historyStudyDiskLifecycle(t, codecs[2])
	raw, err := json.MarshalIndent(study, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("capacity study written without host or hardware identifiers")
}

func historyStudyList(t *testing.T, accounts int) historyStudyListing {
	broker, _ := historyStudyBroker(t)
	const perAccount = 1000
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := broker.store.db.Update(func(tx *bbolt.Tx) error {
		root, err := tx.CreateBucketIfNotExists([]byte("study_account_index"))
		if err != nil {
			return err
		}
		for account := range accounts {
			principal := fmt.Sprintf("principal-%06d", account)
			bucket, err := root.CreateBucket([]byte(principal))
			if err != nil {
				return err
			}
			for i := range perAccount {
				id := fmt.Sprintf("session-%06d-%06d", account, i)
				s := &sessionRecord{Session: runnerproto.Session{ID: id, PrincipalID: principal, State: "closed", CreatedAt: now.Add(time.Duration(i) * time.Minute), UpdatedAt: now.Add(time.Duration(i) * time.Minute), Approvals: []runnerproto.Approval{}, Summaries: []runnerproto.TurnSummary{}}}
				broker.sessions[id] = s
				if err := putJSON(bucket, string(eventKey(uint64(i))), s.Session); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result := historyStudyListing{Accounts: accounts, TotalSessions: accounts * perAccount, PerAccount: perAccount, CurrentVisible: len(broker.list("principal-000000"))}
	result.Current = historyStudyMeasureBatch(t, "current_scan_all_sort_owner_cap_200", 100, 20, func() error { _, err := json.Marshal(broker.list("principal-000000")); return err })
	result.Indexed = historyStudyMeasureBatch(t, "indexed_account_page_50", 100, 50, func() error {
		var sessions []runnerproto.Session
		if err := broker.store.db.View(func(tx *bbolt.Tx) error {
			cursor := tx.Bucket([]byte("study_account_index")).Bucket([]byte("principal-000000")).Cursor()
			for key, raw := cursor.Last(); key != nil && len(sessions) < 50; key, raw = cursor.Prev() {
				var s runnerproto.Session
				if err := json.Unmarshal(raw, &s); err != nil {
					return err
				}
				sessions = append(sessions, s)
			}
			return nil
		}); err != nil {
			return err
		}
		_, err := json.Marshal(sessions)
		return err
	})
	return result
}

func historyStudyConcurrent(t *testing.T, writers, readers int) historyStudyConcurrency {
	broker, _ := historyStudyBroker(t)
	for writer := range writers {
		historyStudySeed(t, broker, fmt.Sprint(writer), fmt.Sprint(writer), historyStudyFixture(300, false))
	}
	const writesPerWorker = 64
	startSignal := make(chan struct{})
	var workers sync.WaitGroup
	latencies := make([]time.Duration, writers*writesPerWorker)
	readLatencies := make([]time.Duration, readers*64)
	errors := make(chan error, writers+readers)
	for writer := range writers {
		workers.Go(func() {
			<-startSignal
			id := fmt.Sprint(writer)
			for i := range writesPerWorker {
				start := time.Now()
				broker.mu.Lock()
				s := copySession(broker.sessions[id])
				s.LastSequence++
				err := broker.saveLocked(s, "", s.PrincipalID, newSessionEvent("turn.delta", map[string]any{"turn_id": "synthetic-turn", "kind": "text", "content": "synthetic concurrent delta"}))
				broker.mu.Unlock()
				if err != nil {
					errors <- err
					return
				}
				latencies[writer*writesPerWorker+i] = time.Since(start)
			}
		})
	}
	for reader := range readers {
		workers.Go(func() {
			<-startSignal
			for i := range 64 {
				start := time.Now()
				page, err := broker.history("0", "0", 0, nil)
				if err == nil {
					_, err = json.Marshal(page)
				}
				if err != nil {
					errors <- err
					return
				}
				readLatencies[reader*64+i] = time.Since(start)
			}
		})
	}
	start := time.Now()
	close(startSignal)
	workers.Wait()
	elapsed := time.Since(start)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	for writer := range writers {
		s := broker.sessions[fmt.Sprint(writer)]
		if s.EventCursor != 300+writesPerWorker || s.LastSequence != writesPerWorker {
			t.Fatal("concurrent event lost")
		}
	}
	result := historyStudyConcurrency{Writers: writers, Readers: readers, Writes: len(latencies), EventsPerSecond: float64(len(latencies)) / elapsed.Seconds(), WriteLatency: historyStudyQuantiles("fsync_broker_append", latencies)}
	if readers > 0 {
		metric := historyStudyQuantiles("history_page_under_write_contention", readLatencies)
		result.ReadLatency = &metric
	}
	return result
}

func historyStudyDiskLifecycle(t *testing.T, codec historyStudyCodec) historyStudyDisk {
	t.Helper()
	broker, path := historyStudyBroker(t)
	events := historyStudyFixture(10000, false)
	s := historyStudySeed(t, broker, "synthetic-session", "synthetic-principal", events)
	archive, err := historyStudyBuildArchive(events, codec, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := historyStudyRestore(archive, archive.Principal, codec); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "synthetic.archive")
	result := historyStudyDisk{}
	result.Publish = historyStudyMeasure(t, "write_archive_and_fsync", 7, func() error {
		file, err := os.OpenFile(archivePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		for _, block := range archive.Blocks {
			if _, err := file.Write(block.data); err != nil {
				return err
			}
		}
		return file.Sync()
	})
	result.Recall = historyStudyMeasure(t, "read_file_verify_decode_all", 20, func() error {
		file, err := os.Open(archivePath)
		if err != nil {
			return err
		}
		defer file.Close()
		for _, block := range archive.Blocks {
			data := make([]byte, block.EncodedBytes)
			if _, err := file.ReadAt(data, block.Offset); err != nil {
				return err
			}
			if _, err := historyStudyDecodeBlock(block, data, codec); err != nil {
				return err
			}
		}
		return nil
	})
	info, _ := os.Stat(path)
	result.BeforeBytes = info.Size()
	info, _ = os.Stat(archivePath)
	result.ArchiveBytes = info.Size()
	if err := broker.store.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte("study_archive_manifests"))
		if err != nil {
			return err
		}
		if err := putJSON(bucket, s.ID, archive); err != nil {
			return err
		}
		return tx.Bucket([]byte(bucketSessionEvents)).DeleteBucket([]byte(s.ID))
	}); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(path)
	result.AfterDeleteBytes = info.Size()
	compactPath := filepath.Join(t.TempDir(), "compact.db")
	// One sample: this is physical file reclamation, not an online replacement procedure.
	start := time.Now()
	destination, err := bbolt.Open(compactPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bbolt.Compact(destination, broker.store.db, 0); err != nil {
		t.Fatal(err)
	}
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}
	result.Compact = historyStudyQuantiles("offline_bolt_compact", []time.Duration{time.Since(start)})
	info, _ = os.Stat(compactPath)
	result.AfterCompactBytes = info.Size()
	return result
}
