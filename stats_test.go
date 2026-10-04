package brotli

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
)

// decodeWithStats decodes data, reading output in chunks of readSize bytes,
// and returns the decoded output along with the collected statistics.
func decodeWithStats(data []byte, readSize int) ([]byte, *DecodeStats, error) {
	r := NewReader(bytes.NewReader(data))
	stats := new(DecodeStats)
	r.SetStatsObserver(stats)

	var out bytes.Buffer
	buf := make([]byte, readSize)
	for {
		n, err := r.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			if err == io.EOF {
				break
			}
			return out.Bytes(), stats, err
		}
	}
	return out.Bytes(), stats, nil
}

// checkStatsInvariants verifies invariants that must hold for any fully
// decoded stream: the classified output of each metablock adds up, and the
// per-metablock output lengths add up to the decompressed length.
func checkStatsInvariants(t *testing.T, stats *DecodeStats, outputLen int) {
	t.Helper()
	if !stats.Complete {
		t.Errorf("stats.Complete = false, want true")
	}
	total := 0
	for i, m := range stats.Metablocks {
		if m.Type == MetablockCompressed {
			sum := m.LiteralBytes + m.CopyBytes + m.DictionaryBytes
			if sum != m.OutputLength {
				t.Errorf("metablock %d: Literal+Copy+Dictionary = %d, want OutputLength %d",
					i, sum, m.OutputLength)
			}
		}
		if m.Type == MetablockMetadata && m.OutputLength != 0 {
			t.Errorf("metablock %d: metadata block produced %d output bytes", i, m.OutputLength)
		}
		if m.DictionaryRefs != len(m.Transforms) {
			t.Errorf("metablock %d: DictionaryRefs = %d, but %d transform IDs recorded",
				i, m.DictionaryRefs, len(m.Transforms))
		}
		total += m.OutputLength
	}
	if total != outputLen {
		t.Errorf("classified output total = %d, want decompressed length %d", total, outputLen)
	}
	if stats.OutputLength() != outputLen {
		t.Errorf("stats.OutputLength() = %d, want %d", stats.OutputLength(), outputLen)
	}
}

// decodeBothWays decodes data in one shot and byte-by-byte, and checks that
// the output and the statistics are identical either way.
func decodeBothWays(t *testing.T, data []byte) ([]byte, *DecodeStats) {
	t.Helper()

	outFull, statsFull, err := decodeWithStats(data, 1<<20)
	if err != nil {
		t.Fatalf("decode with large buffer: %v", err)
	}
	outByte, statsByte, err := decodeWithStats(data, 1)
	if err != nil {
		t.Fatalf("decode byte-by-byte: %v", err)
	}
	if !bytes.Equal(outFull, outByte) {
		t.Fatalf("output differs between read strategies")
	}
	if !reflect.DeepEqual(statsFull.Metablocks, statsByte.Metablocks) {
		t.Fatalf("statistics differ between read strategies:\nlarge buffer: %+v\nbyte-by-byte: %+v",
			statsFull.Metablocks, statsByte.Metablocks)
	}
	checkStatsInvariants(t, statsFull, len(outFull))
	return outFull, statsFull
}

// statsSampleLiteralCopy is highly repetitive, so it is compressed mostly
// with literals and (overlapping) back-reference copies.
var statsSampleLiteralCopy = []byte(strings.Repeat("abcabcabcabd", 500))

// statsSampleDictionary is natural-language text, which the encoder
// compresses partly with static dictionary references.
var statsSampleDictionary = []byte(strings.Repeat(
	"The quick brown fox jumps over the lazy dog. "+
		"Compression is the process of encoding information using fewer bits. ", 20))

func TestDecodeStatsLiteralCopy(t *testing.T) {
	compressed, err := Encode(statsSampleLiteralCopy, WriterOptions{Quality: 5})
	if err != nil {
		t.Fatal(err)
	}

	output, stats := decodeBothWays(t, compressed)
	if !bytes.Equal(output, statsSampleLiteralCopy) {
		t.Fatalf("round-trip mismatch")
	}
	if stats.LiteralBytes() == 0 {
		t.Errorf("no literal bytes recorded")
	}
	if stats.CopyBytes() == 0 {
		t.Errorf("no back-reference copy bytes recorded")
	}
	if stats.LiteralBytes()+stats.CopyBytes()+stats.DictionaryBytes() != len(output) {
		t.Errorf("Literal+Copy+Dictionary = %d, want %d",
			stats.LiteralBytes()+stats.CopyBytes()+stats.DictionaryBytes(), len(output))
	}
}

func TestDecodeStatsDictionary(t *testing.T) {
	compressed, err := Encode(statsSampleDictionary, WriterOptions{Quality: 11})
	if err != nil {
		t.Fatal(err)
	}

	output, stats := decodeBothWays(t, compressed)
	if !bytes.Equal(output, statsSampleDictionary) {
		t.Fatalf("round-trip mismatch")
	}
	if stats.DictionaryRefs() == 0 {
		t.Errorf("no static dictionary references recorded")
	}
	if stats.DictionaryBytes() == 0 {
		t.Errorf("no static dictionary output bytes recorded")
	}
	for i, m := range stats.Metablocks {
		for _, tr := range m.Transforms {
			if tr < 0 {
				t.Errorf("metablock %d: invalid transform ID %d", i, tr)
			}
		}
	}
}

// lsbBitWriter appends bits LSB-first, the way brotli packs them.
type lsbBitWriter struct {
	data   []byte
	bitPos uint
}

func (w *lsbBitWriter) writeBits(value uint64, numBits uint) {
	for i := uint(0); i < numBits; i++ {
		if w.bitPos%8 == 0 {
			w.data = append(w.data, 0)
		}
		if value&(1<<i) != 0 {
			w.data[len(w.data)-1] |= 1 << (w.bitPos % 8)
		}
		w.bitPos++
	}
}

// buildRawStream builds a brotli stream containing one metadata metablock
// and one uncompressed metablock, followed by an empty final metablock.
func buildRawStream(metadata, raw []byte) []byte {
	var w lsbBitWriter
	w.writeBits(0, 1) // WBITS: 16

	// Metadata metablock.
	w.writeBits(0, 1)                       // ISLAST = 0
	w.writeBits(3, 2)                       // MNIBBLES = 3 -> metadata
	w.writeBits(0, 1)                       // reserved
	w.writeBits(1, 2)                       // MSKIPBYTES = 1
	w.writeBits(uint64(len(metadata)-1), 8) // MSKIPLEN - 1
	for w.bitPos%8 != 0 {                   // align to byte boundary
		w.writeBits(0, 1)
	}
	w.data = append(w.data, metadata...)
	w.bitPos = uint(len(w.data)) * 8

	// Uncompressed metablock.
	w.writeBits(0, 1)                   // ISLAST = 0
	w.writeBits(0, 2)                   // MNIBBLES = 0 -> 4 nibbles
	w.writeBits(uint64(len(raw)-1), 16) // MLEN - 1
	w.writeBits(1, 1)                   // ISUNCOMPRESSED = 1
	for w.bitPos%8 != 0 {               // align to byte boundary
		w.writeBits(0, 1)
	}
	w.data = append(w.data, raw...)
	w.bitPos = uint(len(w.data)) * 8

	// Empty final metablock.
	w.writeBits(1, 1) // ISLAST = 1
	w.writeBits(1, 1) // ISLASTEMPTY = 1
	return w.data
}

func TestDecodeStatsUncompressedAndMetadata(t *testing.T) {
	metadata := []byte{0xde, 0xad, 0xbe, 0xef}
	raw := []byte("uncompressed payload, stored verbatim")
	stream := buildRawStream(metadata, raw)

	output, stats := decodeBothWays(t, stream)
	if !bytes.Equal(output, raw) {
		t.Fatalf("output = %q, want %q", output, raw)
	}
	if len(stats.Metablocks) != 2 {
		t.Fatalf("got %d metablocks, want 2: %+v", len(stats.Metablocks), stats.Metablocks)
	}
	if stats.Metablocks[0].Type != MetablockMetadata {
		t.Errorf("metablock 0 type = %v, want metadata", stats.Metablocks[0].Type)
	}
	if stats.Metablocks[1].Type != MetablockUncompressed {
		t.Errorf("metablock 1 type = %v, want uncompressed", stats.Metablocks[1].Type)
	}
	if stats.Metablocks[1].OutputLength != len(raw) {
		t.Errorf("uncompressed metablock output = %d, want %d",
			stats.Metablocks[1].OutputLength, len(raw))
	}
}

func TestDecodeStatsError(t *testing.T) {
	compressed, err := Encode(statsSampleDictionary, WriterOptions{Quality: 11})
	if err != nil {
		t.Fatal(err)
	}
	_, cleanStats, err := decodeWithStats(compressed, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt a byte in the middle of the stream.
	corrupted := bytes.Clone(compressed)
	corrupted[len(corrupted)/2] ^= 0xFF

	_, stats, err := decodeWithStats(corrupted, 1<<20)
	if err == nil {
		t.Fatalf("corrupted stream decoded without error")
	}
	if stats.Complete {
		t.Errorf("stats.Complete = true after error")
	}
	if len(stats.Metablocks) > len(cleanStats.Metablocks) {
		t.Errorf("error stats cover %d metablocks, more than the clean stream's %d",
			len(stats.Metablocks), len(cleanStats.Metablocks))
	}
	// The reported metablocks must be a confirmed prefix of the clean run.
	for i, m := range stats.Metablocks {
		if !reflect.DeepEqual(m, cleanStats.Metablocks[i]) {
			t.Errorf("metablock %d after error = %+v, want prefix entry %+v",
				i, m, cleanStats.Metablocks[i])
		}
	}
}

func TestDecodeStatsTruncated(t *testing.T) {
	compressed, err := Encode(statsSampleLiteralCopy, WriterOptions{Quality: 5})
	if err != nil {
		t.Fatal(err)
	}

	_, stats, err := decodeWithStats(compressed[:len(compressed)/2], 1<<20)
	if err == nil {
		t.Fatalf("truncated stream decoded without error")
	}
	if stats.Complete {
		t.Errorf("stats.Complete = true after truncation")
	}
}

func TestDecodeStatsReset(t *testing.T) {
	compressedA, err := Encode(statsSampleLiteralCopy, WriterOptions{Quality: 5})
	if err != nil {
		t.Fatal(err)
	}
	compressedB, err := Encode(statsSampleDictionary, WriterOptions{Quality: 11})
	if err != nil {
		t.Fatal(err)
	}

	r := NewReader(bytes.NewReader(compressedA))
	stats := new(DecodeStats)
	r.SetStatsObserver(stats)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	if !stats.Complete || len(stats.Metablocks) == 0 {
		t.Fatalf("first stream: Complete=%v, %d metablocks", stats.Complete, len(stats.Metablocks))
	}

	// After Reset, statistics of the first stream must not leak into the
	// second one.
	if err := r.Reset(bytes.NewReader(compressedB)); err != nil {
		t.Fatal(err)
	}
	outB, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	_, freshStats, err := decodeWithStats(compressedB, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stats.Metablocks, freshStats.Metablocks) {
		t.Errorf("stats after Reset = %+v, want fresh stream stats %+v",
			stats.Metablocks, freshStats.Metablocks)
	}
	if stats.OutputLength() != len(outB) {
		t.Errorf("stats after Reset account for %d bytes, output was %d",
			stats.OutputLength(), len(outB))
	}
}

func TestDecodeStatsDisabled(t *testing.T) {
	compressed, err := Encode(statsSampleDictionary, WriterOptions{Quality: 11})
	if err != nil {
		t.Fatal(err)
	}

	// Enabling and then disabling the observer must restore the original
	// decoding behavior.
	r := NewReader(bytes.NewReader(compressed))
	r.SetStatsObserver(new(DecodeStats))
	r.SetStatsObserver(nil)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, statsSampleDictionary) {
		t.Errorf("decode with disabled observer differs")
	}

	// A reader that never had an observer decodes identically.
	out2, err := Decode(compressed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, out2) {
		t.Errorf("decode with observer detached differs from plain decode")
	}
}
