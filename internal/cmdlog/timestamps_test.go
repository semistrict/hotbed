package cmdlog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTimestampWriterAndTailEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cmd.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	var now int64 = 1_700_000_001_000
	tw := NewTimestampWriter(path, f)
	tw.nowFn = func() int64 { return now }

	if _, err := tw.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	now = 1_700_000_002_500
	if _, err := tw.Write([]byte("second part ")); err != nil {
		t.Fatal(err)
	}
	now = 1_700_000_003_750
	if _, err := tw.Write([]byte("done\nthird\n")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	entries, cut, err := TailEntries(path, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if cut {
		t.Fatalf("unexpected cut=true")
	}
	want := []LineEntry{
		{Text: "first", ReceivedAt: 1_700_000_001_000},
		{Text: "second part done", ReceivedAt: 1_700_000_003_750},
		{Text: "third", ReceivedAt: 1_700_000_003_750},
	}
	if len(entries) != len(want) {
		t.Fatalf("TailEntries = %+v, want %+v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entry[%d] = %+v, want %+v", i, entries[i], want[i])
		}
	}

	// SplitChunks divides a single read across timestamp boundaries.
	idx, err := ReadTimestamps(path)
	if err != nil {
		t.Fatal(err)
	}
	all, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	type chunk struct {
		text string
		ts   int64
	}
	var got []chunk
	if err := idx.SplitChunks(0, all, func(_ int64, p []byte, ts int64) error {
		got = append(got, chunk{text: string(p), ts: ts})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wantChunks := []chunk{
		{text: "first\n", ts: 1_700_000_001_000},
		{text: "second part ", ts: 1_700_000_002_500},
		{text: "done\nthird\n", ts: 1_700_000_003_750},
	}
	if len(got) != len(wantChunks) {
		t.Fatalf("SplitChunks = %+v, want %+v", got, wantChunks)
	}
	for i := range wantChunks {
		if got[i] != wantChunks[i] {
			t.Errorf("chunk[%d] = %+v, want %+v", i, got[i], wantChunks[i])
		}
	}

	// Remove deletes both the log and its .ts sidecar.
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tsPath(path)); !os.IsNotExist(err) {
		t.Fatalf("expected .ts sidecar to be removed, stat err = %v", err)
	}
}

func TestRecordIfGrownAndLegacyMissingSidecar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.log")
	if err := os.WriteFile(path, []byte("old-1\nold-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Missing .ts degrades cleanly to 0 timestamps.
	entries, _, err := TailEntries(path, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ReceivedAt != 0 || entries[1].ReceivedAt != 0 {
		t.Fatalf("legacy entries = %+v, want zero timestamps", entries)
	}

	// RecordIfGrown stamps existing bytes and then subsequent appends.
	if _, err := RecordIfGrown(path, 1_700_000_010_000); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("new-3\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := RecordIfGrown(path, 1_700_000_020_000); err != nil {
		t.Fatal(err)
	}

	entries, _, err = TailEntries(path, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].ReceivedAt != 1_700_000_010_000 || entries[1].ReceivedAt != 1_700_000_010_000 || entries[2].ReceivedAt != 1_700_000_020_000 {
		t.Fatalf("entries after RecordIfGrown = %+v", entries)
	}
}
