package ironhawk

import "testing"

func TestParseEmitPrefix_NewFormat(t *testing.T) {
	line := `[emit_epoch=b:0, emit_commit_index=42] val`
	eu, ec, ci, tail, ok := parseEmitPrefix(line)
	if !ok {
		t.Fatalf("expected ok parse")
	}
	if eu != "b" || ec != 0 || ci != 42 || tail != "val" {
		t.Fatalf("got eu=%q ec=%d ci=%d tail=%q", eu, ec, ci, tail)
	}
}

func TestParseEmitPrefix_Legacy(t *testing.T) {
	line := `[emit_commit_index=7] val`
	eu, ec, ci, tail, ok := parseEmitPrefix(line)
	if !ok {
		t.Fatalf("expected ok parse")
	}
	if eu != "" || ec != 0 || ci != 7 || tail != "val" {
		t.Fatalf("got eu=%q ec=%d ci=%d tail=%q", eu, ec, ci, tail)
	}
}

func TestParseEmitPrefix_Malformed(t *testing.T) {
	cases := []string{
		"emit_commit_index=5] val",           // missing opening
		"[emit_commit_index=] val",           // missing value
		"[emit_epoch=x:y, emit_commit_index=a] tail", // bad index
		"[emit_epoch=x] tail",               // missing counter and index
	}
	for _, c := range cases {
		if _, _, ci, _, ok := parseEmitPrefix(c); ok && ci > 0 {
			t.Fatalf("case %q should not parse with commit index", c)
		}
	}
}

func TestShouldProcess_DedupeRules(t *testing.T) {
	ds := NewInMemoryDedupe()
	fp := uint64(123)
	// first message
	if !shouldProcess(fp, "e1", 1, 10, ds) { t.Fatal("first should process") }
	// duplicate
	if shouldProcess(fp, "e1", 1, 10, ds) { t.Fatal("equal index should skip") }
	// higher advances
	if !shouldProcess(fp, "e1", 1, 12, ds) { t.Fatal("higher should process") }
	// lower same epoch skips
	if shouldProcess(fp, "e1", 1, 11, ds) { t.Fatal("lower same epoch should skip") }
	// new epoch with lower index processes (fresh stream)
	if !shouldProcess(fp, "e2", 1, 1, ds) { t.Fatal("new epoch with lower index should process") }
}

func TestDedupe_SmokeSequenceWithEpochRestart(t *testing.T) {
	ds := NewInMemoryDedupe()
	fp := uint64(55)
	// initial stream indices
	seq1 := []uint64{93,95,97}
	for _, idx := range seq1 {
		if !shouldProcess(fp, "epoch-A", 1, idx, ds) {
			t.Fatalf("idx %d should process on epoch-A", idx)
		}
		// duplicates skipped
		if shouldProcess(fp, "epoch-A", 1, idx, ds) {
			t.Fatalf("idx %d duplicate should skip on epoch-A", idx)
		}
	}
	// reconnect/restart with new epoch and low indices
	seq2 := []uint64{1,3}
	for _, idx := range seq2 {
		if !shouldProcess(fp, "epoch-B", 1, idx, ds) {
			t.Fatalf("idx %d should process on epoch-B after restart", idx)
		}
	}
}
