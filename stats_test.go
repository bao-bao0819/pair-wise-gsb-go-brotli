package brotli

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

// statsSample is a fixed, small brotli stream (LGWin=16) containing, in
// order:
//   - an uncompressed metablock (10 high-entropy bytes),
//   - a metadata metablock ("METADATA", produces no output),
//   - a compressed final metablock whose accepted commands mix literals,
//     an overlapping back-reference copy, and static dictionary
//     references with transforms.
//
// It was produced once with the in-package encoder (quality 11, LGWin 16,
// one Flush followed by an operationEmitMetadata block) and recorded here
// so the test never depends on encoder heuristics.
var statsSample = mustDecodeHex("" +
	"900010017f3ca955e2109b44d0d6014d45544144415441510280df488dd4dab3e" +
	"a3b17bab20582d66108524829f29562b6c14fa7a34601")

var statsSamplePlaintext = []byte{
	0x01, 0x7f, 0x3c, 0xa9, 0x55, 0xe2, 0x10, 0x9b, 0x44, 0xd0,
}

func init() {
	statsSamplePlaintext = append(statsSamplePlaintext,
		[]byte("the the the the http://www.example.com")...)
}

func mustDecodeHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

type recordingObserver struct {
	blocks []MetablockStats
	endErr error
	ended  bool
}

func (o *recordingObserver) ObserveMetablock(s MetablockStats) {
	o.blocks = append(o.blocks, s)
}

func (o *recordingObserver) StreamEnd(err error) {
	o.ended = true
	o.endErr = err
}

// readAllWithBuffer decodes r using the given output buffer size. A size
// of 0 means a single large Read; size 1 forces byte-at-a-time decoding,
// resuming the same commands many times.
func readAllWithBuffer(t *testing.T, r *Reader, size int) []byte {
	t.Helper()
	if size <= 0 {
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}
	var out []byte
	buf := make([]byte, size)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return out
}

func assertSampleStats(t *testing.T, blocks []MetablockStats) {
	t.Helper()
	if len(blocks) != 3 {
		t.Fatalf("got %d metablock records, want 3: %+v", len(blocks), blocks)
	}

	if blocks[0].Type != MetablockUncompressed {
		t.Errorf("block 0 type = %v, want MetablockUncompressed", blocks[0].Type)
	}
	if blocks[0].OutputLength != 10 {
		t.Errorf("uncompressed OutputLength = %d, want 10", blocks[0].OutputLength)
	}
	if blocks[0].LiteralBytes != 0 || blocks[0].CopyBytes != 0 || blocks[0].DictionaryBytes != 0 {
		t.Errorf("uncompressed block must have no literals/copies/dict: %+v", blocks[0])
	}
	if !blocks[0].Complete {
		t.Errorf("uncompressed block marked incomplete")
	}

	if blocks[1].Type != MetablockMetadata {
		t.Errorf("block 1 type = %v, want MetablockMetadata", blocks[1].Type)
	}
	if blocks[1].OutputLength != 0 {
		t.Errorf("metadata OutputLength = %d, want 0", blocks[1].OutputLength)
	}

	if blocks[2].Type != MetablockCompressed {
		t.Errorf("block 2 type = %v, want MetablockCompressed", blocks[2].Type)
	}
	c := blocks[2]
	if c.LiteralBytes != 8 {
		t.Errorf("LiteralBytes = %d, want 8", c.LiteralBytes)
	}
	if c.CopyBytes != 12 {
		t.Errorf("CopyBytes = %d, want 12 (overlapping copy counted by output)", c.CopyBytes)
	}
	if c.DictionaryBytes != 18 {
		t.Errorf("DictionaryBytes = %d, want 18", c.DictionaryBytes)
	}
	if c.OutputLength != 38 {
		t.Errorf("compressed OutputLength = %d, want 38", c.OutputLength)
	}
	if sum := c.LiteralBytes + c.CopyBytes + c.DictionaryBytes; sum != c.OutputLength {
		t.Errorf("category sum = %d, want OutputLength %d", sum, c.OutputLength)
	}
	if len(c.DictionaryReferences) != 3 {
		t.Fatalf("DictionaryReferences = %d entries, want 3: %+v",
			len(c.DictionaryReferences), c.DictionaryReferences)
	}
	wantRefs := []DictionaryReference{
		{Length: 7, WordIndex: 1417, Transform: 0},
		{Length: 7, WordIndex: 464, Transform: 0},
		{Length: 4, WordIndex: 187, Transform: 0},
	}
	for i, want := range wantRefs {
		if got := c.DictionaryReferences[i]; got != want {
			t.Errorf("reference %d = %+v, want %+v", i, got, want)
		}
	}

	total := 0
	for _, b := range blocks {
		total += b.OutputLength
	}
	if total != len(statsSamplePlaintext) {
		t.Errorf("total classified output = %d, want plaintext length %d",
			total, len(statsSamplePlaintext))
	}
}

func TestStatsSampleReadAll(t *testing.T) {
	r := NewReader(bytes.NewReader(statsSample))
	obs := &recordingObserver{}
	r.SetStatsObserver(obs)

	got := readAllWithBuffer(t, r, 0)
	if !bytes.Equal(got, statsSamplePlaintext) {
		t.Fatalf("plaintext mismatch:\n got %x\nwant %x", got, statsSamplePlaintext)
	}
	assertSampleStats(t, obs.blocks)
	if !obs.ended || obs.endErr != nil {
		t.Errorf("StreamEnd = %v, %v; want nil error", obs.ended, obs.endErr)
	}
}

func TestStatsSampleByteByByte(t *testing.T) {
	r := NewReader(bytes.NewReader(statsSample))
	obs := &recordingObserver{}
	r.SetStatsObserver(obs)

	got := readAllWithBuffer(t, r, 1)
	if !bytes.Equal(got, statsSamplePlaintext) {
		t.Fatalf("plaintext mismatch:\n got %x\nwant %x", got, statsSamplePlaintext)
	}
	assertSampleStats(t, obs.blocks)
	if !obs.ended || obs.endErr != nil {
		t.Errorf("StreamEnd = %v, %v; want nil error", obs.ended, obs.endErr)
	}
}

func TestStatsSampleSmallBuffers(t *testing.T) {
	// Several non-trivial buffer sizes exercise command resumes (literal
	// flushes, dict copy flushes, overlapping wrap copies) while every
	// accepted command must still be recorded exactly once.
	for _, size := range []int{2, 3, 5, 7, 13, 16, 31, 64} {
		r := NewReader(bytes.NewReader(statsSample))
		obs := &recordingObserver{}
		r.SetStatsObserver(obs)
		got := readAllWithBuffer(t, r, size)
		if !bytes.Equal(got, statsSamplePlaintext) {
			t.Fatalf("size %d: plaintext mismatch", size)
		}
		assertSampleStats(t, obs.blocks)
	}
}

func TestStatsNoObserverUnchanged(t *testing.T) {
	r := NewReader(bytes.NewReader(statsSample))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(got, statsSamplePlaintext) {
		t.Fatalf("plaintext mismatch without observer")
	}
	if r.stats_cur != nil || r.stats_ended {
		t.Errorf("observer-disabled state leaked: cur=%v ended=%v", r.stats_cur, r.stats_ended)
	}
}

func TestStatsResetIsolatesStreams(t *testing.T) {
	r := NewReader(bytes.NewReader(statsSample))
	obs := &recordingObserver{}
	r.SetStatsObserver(obs)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("first decode: %v", err)
	}
	if len(obs.blocks) != 3 {
		t.Fatalf("first stream produced %d blocks", len(obs.blocks))
	}

	// Reset and decode a different stream: no records from the first
	// stream may leak in, and StreamEnd fires once for each stream.
	ends := 0
	if obs.ended {
		ends++
	}
	r.Reset(bytes.NewReader(statsSample))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("second decode: %v", err)
	}
	if !bytes.Equal(got, statsSamplePlaintext) {
		t.Fatalf("second stream plaintext mismatch")
	}
	if len(obs.blocks) != 6 {
		t.Fatalf("after Reset got %d records, want 6 (no mixing)", len(obs.blocks))
	}
	if obs.ended {
		ends++
	}
	if ends != 2 {
		t.Errorf("StreamEnd fired %d times, want 2", ends)
	}
}

func TestStatsRemoveObserver(t *testing.T) {
	r := NewReader(bytes.NewReader(statsSample))
	obs := &recordingObserver{}
	r.SetStatsObserver(obs)
	// Install then remove the observer before decoding anything.
	r.SetStatsObserver(nil)
	if r.stats_observer != nil || r.stats_cur != nil {
		t.Fatal("observer state not cleared on removal")
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(got, statsSamplePlaintext) {
		t.Fatal("plaintext mismatch without observer")
	}
	if len(obs.blocks) != 0 {
		t.Fatalf("removed observer received records: %d", len(obs.blocks))
	}
	if obs.ended {
		t.Fatal("removed observer received StreamEnd")
	}
}

func TestStatsTruncatedStreamReportsPrefix(t *testing.T) {
	// Cut the sample in the middle of the compressed metablock. The
	// observer must receive a final, explicitly incomplete record that
	// covers only the confirmed prefix, and its category sum must match
	// the bytes actually produced.
	for _, cut := range []int{12, 20, 30, 40} {
		if cut >= len(statsSample) {
			continue
		}
		r := NewReader(bytes.NewReader(statsSample[:cut]))
		obs := &recordingObserver{}
		r.SetStatsObserver(obs)
		out, _ := io.ReadAll(r)

		if !obs.ended {
			t.Errorf("cut=%d: StreamEnd not reported", cut)
		}
		if obs.endErr == nil {
			t.Errorf("cut=%d: StreamEnd error is nil, want non-nil", cut)
		}
		if len(obs.blocks) == 0 {
			t.Fatalf("cut=%d: no records at all", cut)
		}
		last := obs.blocks[len(obs.blocks)-1]
		if last.Complete {
			t.Errorf("cut=%d: final record marked complete", cut)
		}
		classified := 0
		for _, b := range obs.blocks {
			classified += b.OutputLength
		}
		if classified != len(out) {
			t.Errorf("cut=%d: classified prefix %d != produced bytes %d",
				cut, classified, len(out))
		}
		// Repeated Read calls after the error must not add records.
		n := len(obs.blocks)
		_, _ = r.Read(make([]byte, 16))
		if len(obs.blocks) != n {
			t.Errorf("cut=%d: post-error Read added records", cut)
		}
	}
}

func TestStatsCorruptedStreamReportsPrefix(t *testing.T) {
	// Corrupt bytes inside the compressed metablock. Decoding fails; the
	// observer still sees the metadata and uncompressed records plus, at
	// most, one incomplete compressed record, and StreamEnd carries the
	// decode error exactly once.
	bad := bytes.Clone(statsSample)
	bad[len(bad)-3] ^= 0xff

	for _, size := range []int{0, 1, 7} {
		r := NewReader(bytes.NewReader(bad))
		obs := &recordingObserver{}
		r.SetStatsObserver(obs)
		out, err := readAllWithBufferNoFail(t, r, size)
		if err == nil {
			t.Fatalf("size=%d: expected decode error", size)
		}
		if !obs.ended || obs.endErr == nil {
			t.Fatalf("size=%d: StreamEnd not reported with error", size)
		}

		classified := 0
		incomplete := 0
		for _, b := range obs.blocks {
			classified += b.OutputLength
			if !b.Complete {
				incomplete++
			}
		}
		if incomplete > 1 {
			t.Errorf("size=%d: %d incomplete records, want <= 1", size, incomplete)
		}
		// Everything handed to the caller is covered by the records; the
		// final record is explicitly marked incomplete.
		if classified < len(out) {
			t.Errorf("size=%d: classified %d < produced %d",
				size, classified, len(out))
		}

		n := len(obs.blocks)
		_, _ = r.Read(make([]byte, 16))
		if len(obs.blocks) != n {
			t.Errorf("size=%d: post-error Read added records", size)
		}
	}
}

func readAllWithBufferNoFail(t *testing.T, r *Reader, size int) ([]byte, error) {
	t.Helper()
	if size <= 0 {
		return io.ReadAll(r)
	}
	var out []byte
	buf := make([]byte, size)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}
