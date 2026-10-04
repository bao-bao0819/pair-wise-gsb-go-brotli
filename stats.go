package brotli

// MetablockType classifies a decoded metablock by the way its bytes are
// produced.
type MetablockType int

const (
	// MetablockCompressed holds Huffman-coded insert-and-copy commands.
	MetablockCompressed MetablockType = iota
	// MetablockUncompressed is an uncompressed metablock: its bytes are
	// copied verbatim, without literals or back-references.
	MetablockUncompressed
	// MetablockMetadata is a metadata metablock; it produces no output.
	MetablockMetadata
)

// DictionaryReference describes a single accepted command that copies data
// from Brotli's built-in static dictionary instead of the sliding window.
type DictionaryReference struct {
	// Length is the number of output bytes the reference produced, after
	// applying the transform. It can differ from the coded copy length.
	Length int
	// WordIndex identifies the dictionary word of the coded copy length.
	WordIndex int
	// Transform is the applied static dictionary transform index
	// (0 is the identity transform).
	Transform int
}

// MetablockStats summarizes the data sources of one decoded metablock.
//
// The numbers are collected from accepted decode commands only, not from
// comparing compressed and decompressed sizes. For a complete metablock,
// LiteralBytes + CopyBytes + DictionaryBytes equals OutputLength.
type MetablockStats struct {
	// Type classifies the metablock as compressed, uncompressed, or
	// metadata.
	Type MetablockType
	// OutputLength is the number of decompressed bytes the metablock
	// contributed. It is always 0 for metadata metablocks.
	OutputLength int
	// LiteralBytes counts bytes emitted as explicit literals.
	LiteralBytes int
	// CopyBytes counts bytes produced by back-references into the
	// sliding window. Overlapping copies are counted by the number of
	// bytes actually written to the output.
	CopyBytes int
	// DictionaryBytes counts bytes produced by static dictionary
	// references, including any bytes added by transforms.
	DictionaryBytes int
	// DictionaryReferences lists the accepted static dictionary
	// references, with word index and transform identifier.
	DictionaryReferences []DictionaryReference
	// Complete reports whether the whole metablock was decoded. On a
	// decode error, the observer may receive one final partial record
	// covering only the confirmed prefix; Complete is false then.
	Complete bool
}

// StatsObserver receives compression-effectiveness statistics while a
// stream is decoded. Callbacks run synchronously from Reader.Read.
//
// ObserveMetablock is called once when a metablock finishes, and a final
// time with a partial record if decoding fails in the middle of one.
// StreamEnd is called exactly once, after the last complete record of a
// successfully decoded stream, or after the partial record of a failed
// one.
type StatsObserver interface {
	ObserveMetablock(stats MetablockStats)
	StreamEnd(err error)
}

// SetStatsObserver installs an observer that records per-metablock
// statistics collected from the commands actually accepted by the decoder.
// Passing nil removes any previously installed observer.
//
// Installing or removing an observer does not change decoding results;
// with no observer installed the decoder behaves exactly as before.
// The observer is retained by Reset, but statistics from a previous stream
// are never mixed into the new one.
func (r *Reader) SetStatsObserver(observer StatsObserver) {
	r.stats_observer = observer
	r.statsReset()
}

func (r *Reader) statsReset() {
	r.stats_cur = nil
	r.stats_ended = false
	r.stats_cmd_literals = 0
	r.stats_cmd_copy = 0
}

// statsBeginMetablock is called once the metablock header is fully decoded.
func (r *Reader) statsBeginMetablock(typ MetablockType) {
	if r.stats_observer == nil || r.stats_ended {
		return
	}
	r.stats_cur = &MetablockStats{Type: typ, Complete: true}
	r.stats_cmd_literals = 0
	r.stats_cmd_copy = 0
}

// statsBeginCommand marks the start of a freshly decoded command.
func (r *Reader) statsBeginCommand() {
	if r.stats_observer == nil || r.stats_cur == nil {
		return
	}
	r.stats_cmd_literals = 0
	r.stats_cmd_copy = 0
}

// statsLiteralByte is called for each literal byte accepted into the
// ring buffer.
func (r *Reader) statsLiteralByte() {
	if r.stats_observer != nil && r.stats_cur != nil {
		r.stats_cmd_literals++
	}
}

// statsUncompressedByte is called for each byte of an uncompressed metablock
// accepted into the ring buffer.
func (r *Reader) statsUncompressedByte(n int) {
	if r.stats_observer != nil && r.stats_cur != nil {
		r.stats_cur.OutputLength += n
	}
}

// statsCommitCommand folds the finished command into the current metablock.
// dictionary is nil for plain back-reference and literal-only commands.
func (r *Reader) statsCommitCommand(dictionary *DictionaryReference) {
	if r.stats_observer == nil || r.stats_cur == nil {
		return
	}
	s := r.stats_cur
	s.LiteralBytes += r.stats_cmd_literals
	s.OutputLength += r.stats_cmd_literals
	if dictionary != nil {
		s.DictionaryBytes += dictionary.Length
		s.OutputLength += dictionary.Length
		ref := *dictionary
		s.DictionaryReferences = append(s.DictionaryReferences, ref)
	} else if r.stats_cmd_copy > 0 {
		s.CopyBytes += r.stats_cmd_copy
		s.OutputLength += r.stats_cmd_copy
	}
	r.stats_cmd_literals = 0
	r.stats_cmd_copy = 0
}

// statsFinishMetablock emits the record for a fully decoded metablock.
func (r *Reader) statsFinishMetablock() {
	if r.stats_observer == nil || r.stats_cur == nil {
		return
	}
	r.stats_cur.Complete = true
	r.stats_observer.ObserveMetablock(*r.stats_cur)
	r.stats_cur = nil
}

// statsStreamEnd finalizes observation. On error, any partially decoded
// metablock is reported once, covering only the confirmed prefix. It is
// safe to call repeatedly; only the first call per stream is observed.
func (r *Reader) statsStreamEnd(err error) {
	if r.stats_observer == nil || r.stats_ended {
		return
	}
	r.stats_ended = true
	if err != nil && r.stats_cur != nil {
		r.stats_cur.Complete = false
		r.stats_observer.ObserveMetablock(*r.stats_cur)
		r.stats_cur = nil
	}
	r.stats_observer.StreamEnd(err)
}
