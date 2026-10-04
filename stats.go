package brotli

// MetablockType identifies the kind of a brotli metablock.
type MetablockType int

const (
	// MetablockCompressed is a regular compressed metablock, decoded as a
	// sequence of insert-and-copy commands.
	MetablockCompressed MetablockType = iota
	// MetablockUncompressed is a metablock whose payload is stored verbatim.
	MetablockUncompressed
	// MetablockMetadata is a metadata metablock; it produces no output.
	MetablockMetadata
)

func (t MetablockType) String() string {
	switch t {
	case MetablockCompressed:
		return "compressed"
	case MetablockUncompressed:
		return "uncompressed"
	case MetablockMetadata:
		return "metadata"
	default:
		return "unknown"
	}
}

// MetablockStats describes the decoded output of a single metablock.
//
// The counters are derived from the decode commands actually accepted by the
// decoder, not from a comparison of compressed and uncompressed sizes. A
// command that is resumed several times because of a small output buffer is
// accounted exactly once, and overlapping copies are accounted by the number
// of bytes they actually emit.
type MetablockStats struct {
	Type MetablockType

	// OutputLength is the total number of output bytes produced by this
	// metablock. It is zero for metadata metablocks.
	OutputLength int

	// LiteralBytes is the number of output bytes decoded from literal
	// symbols (compressed metablocks only).
	LiteralBytes int

	// CopyBytes is the number of output bytes produced by LZ77
	// back-reference copies (compressed metablocks only).
	CopyBytes int

	// DictionaryBytes is the number of output bytes produced by static
	// dictionary references, after applying the word transform.
	DictionaryBytes int

	// DictionaryRefs is the number of static dictionary references used by
	// this metablock.
	DictionaryRefs int

	// Transforms holds the transform identifier of each static dictionary
	// reference, in the order the references were decoded.
	Transforms []int
}

// StatsObserver receives decode statistics from a Reader. Use
// Reader.SetStatsObserver to attach one.
//
// ObserveMetablock is called once per metablock, after the metablock has been
// fully decoded. Metablocks that are only partially decoded when an error
// occurs are never reported, so the reported metablocks always form a
// confirmed prefix of the stream.
type StatsObserver interface {
	ObserveMetablock(m MetablockStats)
}

// statsCompletionObserver is implemented by observers that want to be
// notified when a stream has been decoded completely and successfully.
type statsCompletionObserver interface {
	ObserveComplete()
}

// statsResetObserver is implemented by observers that want their collected
// statistics to be discarded when the Reader is reset.
type statsResetObserver interface {
	ResetStats()
}

// DecodeStats is a StatsObserver that collects the statistics of every
// metablock of one decoded stream.
type DecodeStats struct {
	// Metablocks holds one entry per fully decoded metablock, in stream
	// order.
	Metablocks []MetablockStats

	// Complete reports whether the stream was decoded completely and
	// successfully. If decoding fails, Complete is false and Metablocks
	// covers only the confirmed prefix of the stream.
	Complete bool
}

// ObserveMetablock implements StatsObserver.
func (s *DecodeStats) ObserveMetablock(m MetablockStats) {
	s.Metablocks = append(s.Metablocks, m)
}

// ObserveComplete marks the stream as completely decoded.
func (s *DecodeStats) ObserveComplete() {
	s.Complete = true
}

// ResetStats discards all statistics collected so far. It is called by
// Reader.Reset so that statistics of a previous stream are not mixed into
// the next one.
func (s *DecodeStats) ResetStats() {
	s.Metablocks = s.Metablocks[:0]
	s.Complete = false
}

// OutputLength returns the total number of output bytes accounted for by the
// collected metablocks.
func (s *DecodeStats) OutputLength() int {
	total := 0
	for i := range s.Metablocks {
		total += s.Metablocks[i].OutputLength
	}
	return total
}

// LiteralBytes returns the total number of literal output bytes.
func (s *DecodeStats) LiteralBytes() int {
	total := 0
	for i := range s.Metablocks {
		total += s.Metablocks[i].LiteralBytes
	}
	return total
}

// CopyBytes returns the total number of output bytes produced by LZ77
// back-reference copies.
func (s *DecodeStats) CopyBytes() int {
	total := 0
	for i := range s.Metablocks {
		total += s.Metablocks[i].CopyBytes
	}
	return total
}

// DictionaryBytes returns the total number of output bytes produced by
// static dictionary references.
func (s *DecodeStats) DictionaryBytes() int {
	total := 0
	for i := range s.Metablocks {
		total += s.Metablocks[i].DictionaryBytes
	}
	return total
}

// DictionaryRefs returns the total number of static dictionary references.
func (s *DecodeStats) DictionaryRefs() int {
	total := 0
	for i := range s.Metablocks {
		total += s.Metablocks[i].DictionaryRefs
	}
	return total
}

// SetStatsObserver attaches a statistics observer to r. Statistics are
// collected from the decode commands actually accepted by the decoder.
// Passing nil detaches the observer and restores the original decoding
// behavior.
func (r *Reader) SetStatsObserver(obs StatsObserver) {
	r.stats_observer = obs
	if obs == nil {
		r.stats_pending = MetablockStats{}
		r.stats_pending_active = false
	}
}

// statsBeginMetablock starts accumulating statistics for a new metablock.
func (s *Reader) statsBeginMetablock(t MetablockType) {
	if s.stats_observer == nil {
		return
	}
	s.stats_pending = MetablockStats{Type: t}
	s.stats_pending_active = true
}

// statsEndMetablock reports the statistics of the metablock that has just
// been fully decoded.
func (s *Reader) statsEndMetablock() {
	if !s.stats_pending_active {
		return
	}
	s.stats_pending_active = false
	s.stats_observer.ObserveMetablock(s.stats_pending)
	s.stats_pending = MetablockStats{}
}

// statsNotifyComplete notifies the observer that the stream has been decoded
// completely and successfully.
func (s *Reader) statsNotifyComplete() {
	if s.stats_completed || s.stats_observer == nil {
		return
	}
	s.stats_completed = true
	if c, ok := s.stats_observer.(statsCompletionObserver); ok {
		c.ObserveComplete()
	}
}
