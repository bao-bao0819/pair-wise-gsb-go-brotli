package brotli

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
)

// DictMatch describes one way a prefix of the input can be produced from the
// static dictionary: base word WordID (index within its length class) of
// WordLen bytes, rewritten by transform Transform, yielding MatchLen bytes.
type DictMatch struct {
	WordID    int // index of the base word within its length class
	WordLen   int // length of the untransformed dictionary word (4..24)
	Transform int // transform index, 0..numTransforms-1
	MatchLen  int // length of the transformed word = matched prefix length
}

// StaticDictIndexStats reports the size and construction cost of an index.
type StaticDictIndexStats struct {
	WordCount       int // number of base dictionary words indexed
	TransformCount  int // number of transforms applied per word
	CandidateCount  int // number of legal (word, transform) pairs indexed
	BucketCount     int // number of distinct 4-byte prefix buckets
	ShortCandidates int // candidates shorter than 4 bytes (kept aside)
	ApproxBytes     int // approximate memory footprint of the index in bytes
}

// StaticDictIndex is a precomputed prefix index over all legal
// (dictionary word, transform) pairs of the built-in static dictionary.
// It covers all 121 transforms from RFC 7932 section 8, including
// prefix/suffix affixes, omit-first/omit-last truncation and the UTF-8
// uppercase transforms (which follow the format's simplified model, not
// Unicode ToUpper). Build it once with NewStaticDictIndex and query it
// concurrently.
//
// Scale: the dictionary holds 13504 words (lengths 4..24), so the index
// holds at most 13504*121 = 1.63M candidates (about 1.58M after dropping
// illegal omit-transform combinations). Each candidate is one uint64
// (4-byte bucket key plus packed 29-bit payload), so the index uses roughly
// 13 MB and is built once, typically in well under a second
// (O(words*transforms) transform applications plus one sort). After
// construction the index is read-only and safe for concurrent Lookup and
// Reconstruct calls.
type StaticDictIndex struct {
	// entries holds one uint64 per candidate: bucket key (first 4 bytes of
	// the transformed word, little endian) in the high 32 bits, packed
	// candidate payload in the low 32 bits. Sorted ascending.
	entries []uint64
	// short indexes candidates whose transformed length is 1..3 bytes and
	// therefore cannot use a 4-byte bucket key. The key packs the candidate
	// length and its (zero-padded) bytes: uint64(matchLen)<<32 | le32.
	short map[uint64][]uint32

	words *dictionary
	trans *transforms
	stats StaticDictIndexStats
}

const (
	// dictQueryMaxTransformedLen bounds prefix (<=8) + word (<=24) + suffix (<=8).
	dictQueryMaxTransformedLen = 48
	// dictQueryKeyLen is the bucket key width in bytes.
	dictQueryKeyLen = 4
)

// Packed candidate payload layout (29 bits used):
// bits 0..5 match length, bits 6..10 word length, bits 11..17 transform
// index, bits 18..28 word id.
func packDictCandidate(wordID, wordLen, transform, matchLen int) uint32 {
	return uint32(matchLen) |
		uint32(wordLen)<<6 |
		uint32(transform)<<11 |
		uint32(wordID)<<18
}

func unpackDictCandidate(p uint32) (wordID, wordLen, transform, matchLen int) {
	return int(p >> 18), int(p>>6) & 0x1F, int(p>>11) & 0x7F, int(p) & 0x3F
}

// validDictTransform reports whether transform may legally be applied to a
// word of wordLen bytes. Omit transforms that would consume the whole word
// (or more) are rejected; everything else follows the format definition.
func validDictTransform(wordLen, transform int, trans *transforms) bool {
	t := int(transformType(trans, transform))
	switch {
	case t >= transformOmitLast1 && t <= transformOmitLast9:
		return wordLen > t
	case t >= transformOmitFirst1 && t <= transformOmitFirst9:
		return wordLen > t - (transformOmitFirst1 - 1)
	}
	return true
}

// shortKey packs a 1..3 byte transformed word into a short-candidate key.
func shortKey(b []byte) uint64 {
	var k uint32
	for i := 0; i < len(b) && i < dictQueryKeyLen; i++ {
		k |= uint32(b[i]) << uint(8*i)
	}
	return uint64(len(b))<<32 | uint64(k)
}

// NewStaticDictIndex builds the reverse index over the built-in static
// dictionary. Construction applies every transform to every word once
// (about 1.6M transform operations) and sorts the result; the returned
// index is immutable and safe for concurrent use.
func NewStaticDictIndex() *StaticDictIndex {
	words := getDictionary()
	trans := getTransforms()
	idx := &StaticDictIndex{words: words, trans: trans, short: make(map[uint64][]uint32)}

	var buf [dictQueryMaxTransformedLen]byte
	wordCount := 0
	for wordLen := minDictionaryWordLength; wordLen <= maxDictionaryWordLength; wordLen++ {
		n := 1 << words.size_bits_by_length[wordLen]
		offset := int(words.offsets_by_length[wordLen])
		wordCount += n
		for id := 0; id < n; id++ {
			word := words.data[offset+id*wordLen:]
			for tr := 0; tr < int(trans.num_transforms); tr++ {
				if !validDictTransform(wordLen, tr, trans) {
					continue
				}
				matchLen := transformDictionaryWord(buf[:], word, wordLen, trans, tr)
				packed := packDictCandidate(id, wordLen, tr, matchLen)
				if matchLen >= dictQueryKeyLen {
					key := binary.LittleEndian.Uint32(buf[:dictQueryKeyLen])
					idx.entries = append(idx.entries, uint64(key)<<32|uint64(packed))
				} else {
					k := shortKey(buf[:matchLen])
					idx.short[k] = append(idx.short[k], packed)
				}
			}
		}
	}
	sort.Slice(idx.entries, func(i, j int) bool { return idx.entries[i] < idx.entries[j] })

	buckets := 0
	for i, e := range idx.entries {
		if i == 0 || e>>32 != idx.entries[i-1]>>32 {
			buckets++
		}
	}
	shortCount, shortBytes := 0, 0
	for _, list := range idx.short {
		shortCount += len(list)
		shortBytes += len(list) * 4
	}
	idx.stats = StaticDictIndexStats{
		WordCount:       wordCount,
		TransformCount:  int(trans.num_transforms),
		CandidateCount:  len(idx.entries) + shortCount,
		BucketCount:     buckets + len(idx.short),
		ShortCandidates: shortCount,
		ApproxBytes:     len(idx.entries)*8 + shortBytes,
	}
	return idx
}

// Stats returns size and construction-cost information about the index.
func (idx *StaticDictIndex) Stats() StaticDictIndexStats {
	return idx.stats
}

// lessDictMatch is the deterministic, total order used for Lookup results:
// longest match first, then by word length, word id and transform index.
func lessDictMatch(a, b DictMatch) bool {
	if a.MatchLen != b.MatchLen {
		return a.MatchLen > b.MatchLen
	}
	if a.WordLen != b.WordLen {
		return a.WordLen < b.WordLen
	}
	if a.WordID != b.WordID {
		return a.WordID < b.WordID
	}
	return a.Transform < b.Transform
}

// Lookup returns every (word, transform) pair whose transformed output is a
// prefix of data. All sources of an identical byte string are kept; results
// are deterministically sorted (longest match first, then word length, word
// id, transform index). Safe for concurrent use.
func (idx *StaticDictIndex) Lookup(data []byte) []DictMatch {
	var matches []DictMatch
	var buf [dictQueryMaxTransformedLen]byte

	check := func(packed uint32) {
		wordID, wordLen, transform, matchLen := unpackDictCandidate(packed)
		if matchLen > len(data) {
			return
		}
		offset := int(idx.words.offsets_by_length[wordLen]) + wordID*wordLen
		n := transformDictionaryWord(buf[:], idx.words.data[offset:], wordLen, idx.trans, transform)
		if n == matchLen && bytes.Equal(buf[:matchLen], data[:matchLen]) {
			matches = append(matches, DictMatch{wordID, wordLen, transform, matchLen})
		}
	}

	if len(data) >= dictQueryKeyLen {
		key := uint64(binary.LittleEndian.Uint32(data[:dictQueryKeyLen])) << 32
		lo := sort.Search(len(idx.entries), func(i int) bool { return idx.entries[i] >= key })
		for ; lo < len(idx.entries) && idx.entries[lo]>>32 == key>>32; lo++ {
			check(uint32(idx.entries[lo]))
		}
	}
	for l := 1; l < dictQueryKeyLen && l <= len(data); l++ {
		for _, packed := range idx.short[shortKey(data[:l])] {
			check(packed)
		}
	}

	sort.Slice(matches, func(i, j int) bool { return lessDictMatch(matches[i], matches[j]) })
	return matches
}

// Reconstruct regenerates the matched bytes for a (word id, word length,
// transform) triple, exactly as the decoder would. Unknown ids and illegal
// combinations (word length outside 4..24, word id out of range for its
// length class, unknown transform index, or an omit transform that would
// consume the whole word) are reported as errors.
func (idx *StaticDictIndex) Reconstruct(wordID, wordLen, transform int) ([]byte, error) {
	if wordLen < minDictionaryWordLength || wordLen > maxDictionaryWordLength {
		return nil, fmt.Errorf("brotli: invalid dictionary word length %d (must be %d..%d)",
			wordLen, minDictionaryWordLength, maxDictionaryWordLength)
	}
	if n := 1 << idx.words.size_bits_by_length[wordLen]; wordID < 0 || wordID >= n {
		return nil, fmt.Errorf("brotli: word id %d out of range for length %d (must be 0..%d)",
			wordID, wordLen, n-1)
	}
	if transform < 0 || transform >= int(idx.trans.num_transforms) {
		return nil, fmt.Errorf("brotli: unknown transform index %d (must be 0..%d)",
			transform, idx.trans.num_transforms-1)
	}
	if !validDictTransform(wordLen, transform, idx.trans) {
		return nil, fmt.Errorf("brotli: transform %d is not legal for a word of length %d",
			transform, wordLen)
	}
	offset := int(idx.words.offsets_by_length[wordLen]) + wordID*wordLen
	var buf [dictQueryMaxTransformedLen]byte
	n := transformDictionaryWord(buf[:], idx.words.data[offset:], wordLen, idx.trans, transform)
	out := make([]byte, n)
	copy(out, buf[:n])
	return out, nil
}

var sharedDictIndex struct {
	once sync.Once
	idx  *StaticDictIndex
}

// SharedStaticDictIndex returns a lazily built, process-wide index for
// callers that just want to run queries without managing the index lifetime.
func SharedStaticDictIndex() *StaticDictIndex {
	sharedDictIndex.once.Do(func() {
		sharedDictIndex.idx = NewStaticDictIndex()
	})
	return sharedDictIndex.idx
}
