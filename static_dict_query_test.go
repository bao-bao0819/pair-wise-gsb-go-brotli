package brotli

import (
	"bytes"
	"reflect"
	"sort"
	"sync"
	"testing"
)

// naiveDictLookup is the reference implementation: it enumerates every
// (word, transform) pair of the static dictionary and keeps the ones whose
// transformed output is a prefix of data. It deliberately does not use any
// index structure.
func naiveDictLookup(data []byte) []DictMatch {
	words := getDictionary()
	trans := getTransforms()
	var buf [dictQueryMaxTransformedLen]byte
	var matches []DictMatch
	for wordLen := minDictionaryWordLength; wordLen <= maxDictionaryWordLength; wordLen++ {
		n := 1 << words.size_bits_by_length[wordLen]
		offset := int(words.offsets_by_length[wordLen])
		for id := 0; id < n; id++ {
			word := words.data[offset+id*wordLen:]
			for tr := 0; tr < int(trans.num_transforms); tr++ {
				if !validDictTransform(wordLen, tr, trans) {
					continue
				}
				l := transformDictionaryWord(buf[:], word, wordLen, trans, tr)
				if l <= len(data) && bytes.Equal(buf[:l], data[:l]) {
					matches = append(matches, DictMatch{id, wordLen, tr, l})
				}
			}
		}
	}
	sort.Slice(matches, func(i, j int) bool { return lessDictMatch(matches[i], matches[j]) })
	return matches
}

func TestStaticDictLookupAgainstNaive(t *testing.T) {
	idx := NewStaticDictIndex()
	inputs := []string{
		"time",
		"times and seasons",
		"Time flies",
		"DOWNLOAD",
		"information is",
		"tim",
		"ownload",
		"zzz not a dict word",
		"",
		"ab",
	}
	for _, input := range inputs {
		got := idx.Lookup([]byte(input))
		want := naiveDictLookup([]byte(input))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Lookup(%q) = %v, want %v", input, got, want)
		}
	}
}

// findDictWord returns the id of the first word of the given length for
// which pred reports true, or -1.
func findDictWord(wordLen int, pred func(word []byte) bool) int {
	words := getDictionary()
	n := 1 << words.size_bits_by_length[wordLen]
	offset := int(words.offsets_by_length[wordLen])
	for id := 0; id < n; id++ {
		if pred(words.data[offset+id*wordLen : offset+(id+1)*wordLen]) {
			return id
		}
	}
	return -1
}

// findTransform returns the index of the first transform with the given
// type and empty prefix and suffix, or -1.
func findTransform(type_ int) int {
	trans := getTransforms()
	for tr := 0; tr < int(trans.num_transforms); tr++ {
		if int(transformType(trans, tr)) == type_ &&
			transformPrefix(trans, tr)[0] == 0 &&
			transformSuffix(trans, tr)[0] == 0 {
			return tr
		}
	}
	return -1
}

func TestStaticDictReconstructAndReverseLookup(t *testing.T) {
	idx := NewStaticDictIndex()
	words := getDictionary()
	trans := getTransforms()

	// Candidates drawn from the real dictionary: a long word for omit-last
	// truncation, and a word containing a multi-byte UTF-8 sequence for the
	// case transforms.
	longWordID := findDictWord(12, func(w []byte) bool { return true })
	multiByteID := -1
	multiByteLen := 0
	for wordLen := minDictionaryWordLength; wordLen <= maxDictionaryWordLength && multiByteID < 0; wordLen++ {
		multiByteID = findDictWord(wordLen, func(w []byte) bool {
			return bytes.IndexByte(w, 0xC0) >= 0 || bytes.IndexByte(w, 0xC2) >= 0 ||
				bytes.IndexByte(w, 0xC3) >= 0 || bytes.IndexByte(w, 0xE0) >= 0
		})
		if multiByteID >= 0 {
			multiByteLen = wordLen
		}
	}
	if longWordID < 0 || multiByteID < 0 {
		t.Fatal("could not find suitable dictionary words for the test")
	}

	type candidate struct {
		wordID    int
		wordLen   int
		transform int
	}
	var candidates []candidate
	// All transforms for the multi-byte word (covers uppercase-first/all on
	// multi-byte input, truncation, affixes).
	for tr := 0; tr < int(trans.num_transforms); tr++ {
		if validDictTransform(multiByteLen, tr, trans) {
			candidates = append(candidates, candidate{multiByteID, multiByteLen, tr})
		}
	}
	// All transforms for the 12-byte word (covers deep omit-last cuts).
	for tr := 0; tr < int(trans.num_transforms); tr++ {
		if validDictTransform(12, tr, trans) {
			candidates = append(candidates, candidate{longWordID, 12, tr})
		}
	}

	for _, c := range candidates {
		reconstructed, err := idx.Reconstruct(c.wordID, c.wordLen, c.transform)
		if err != nil {
			t.Fatalf("Reconstruct(%d, %d, %d): %v", c.wordID, c.wordLen, c.transform, err)
		}
		// The reconstructed bytes must be found by the reverse query.
		found := false
		for _, m := range idx.Lookup(reconstructed) {
			if m.WordID == c.wordID && m.WordLen == c.wordLen &&
				m.Transform == c.transform && m.MatchLen == len(reconstructed) {
				found = true
				break
			}
		}
		if !found {
			offset := int(words.offsets_by_length[c.wordLen]) + c.wordID*c.wordLen
			t.Errorf("Lookup(%q) missing source (id=%d len=%d transform=%d), word=%q",
				reconstructed, c.wordID, c.wordLen, c.transform,
				words.data[offset:offset+c.wordLen])
		}
	}
}

func TestStaticDictReconstructErrors(t *testing.T) {
	idx := NewStaticDictIndex()
	omitLast9 := findTransform(transformOmitLast9)
	omitFirst9 := findTransform(transformOmitFirst9)
	if omitLast9 < 0 || omitFirst9 < 0 {
		t.Fatal("empty-affix omit transforms not found")
	}
	cases := []struct {
		wordID, wordLen, transform int
	}{
		{0, 3, 0},           // word length too small
		{0, 25, 0},          // word length too large
		{-1, 4, 0},          // negative word id
		{1 << 10, 4, 0},     // word id out of range for length 4
		{0, 4, -1},          // negative transform
		{0, 4, 121},         // unknown transform
		{0, 4, omitLast9},   // omit-last 9 on a 4-byte word
		{0, 4, omitFirst9},  // omit-first 9 on a 4-byte word
	}
	for _, c := range cases {
		if _, err := idx.Reconstruct(c.wordID, c.wordLen, c.transform); err == nil {
			t.Errorf("Reconstruct(%d, %d, %d) succeeded, want error",
				c.wordID, c.wordLen, c.transform)
		}
	}
	if _, err := idx.Reconstruct(0, 24, 0); err != nil {
		t.Errorf("Reconstruct(0, 24, 0) failed: %v", err)
	}
}

func TestStaticDictDuplicateSources(t *testing.T) {
	idx := NewStaticDictIndex()
	words := getDictionary()

	// A word whose first byte is not a lowercase ASCII letter is unchanged
	// by the uppercase-first transform, so identity and uppercase-first
	// (with empty affixes) produce the same bytes and must both be kept.
	upperFirst := findTransform(transformUppercaseFirst)
	if upperFirst < 0 {
		t.Fatal("no empty-affix uppercase-first transform found")
	}
	dupID, dupLen := -1, 0
	for wordLen := minDictionaryWordLength; wordLen <= maxDictionaryWordLength && dupID < 0; wordLen++ {
		dupID = findDictWord(wordLen, func(w []byte) bool {
			return w[0] < 'a' || w[0] > 'z'
		})
		if dupID >= 0 {
			dupLen = wordLen
		}
	}
	if dupID < 0 {
		t.Fatal("no dictionary word with non-lowercase first byte found")
	}
	offset := int(words.offsets_by_length[dupLen]) + dupID*dupLen
	word := words.data[offset : offset+dupLen]

	matches := idx.Lookup(word)
	var gotIdentity, gotUpper bool
	for _, m := range matches {
		if m.WordID == dupID && m.WordLen == dupLen && m.MatchLen == dupLen {
			if m.Transform == 0 {
				gotIdentity = true
			}
			if m.Transform == upperFirst {
				gotUpper = true
			}
		}
	}
	if !gotIdentity || !gotUpper {
		t.Errorf("Lookup(%q): identity=%v uppercase-first=%v, want both sources kept",
			word, gotIdentity, gotUpper)
	}

	// Results must be deterministically sorted on every call.
	again := idx.Lookup(word)
	if !reflect.DeepEqual(matches, again) {
		t.Errorf("Lookup(%q) not deterministic: %v vs %v", word, matches, again)
	}
	for i := 1; i < len(matches); i++ {
		if lessDictMatch(matches[i], matches[i-1]) {
			t.Errorf("Lookup(%q) results out of order at %d: %v", word, i, matches)
			break
		}
	}
}

func TestStaticDictConcurrentLookup(t *testing.T) {
	idx := SharedStaticDictIndex()
	inputs := [][]byte{
		[]byte("time and again"),
		[]byte("DOWNLOAD"),
		[]byte("information"),
		[]byte("tim"),
	}
	want := make([][]DictMatch, len(inputs))
	for i, in := range inputs {
		want[i] = idx.Lookup(in)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i, in := range inputs {
				if got := idx.Lookup(in); !reflect.DeepEqual(got, want[i]) {
					t.Errorf("concurrent Lookup(%q) = %v, want %v", in, got, want[i])
				}
			}
		}()
	}
	wg.Wait()
}

func TestStaticDictIndexStats(t *testing.T) {
	idx := NewStaticDictIndex()
	stats := idx.Stats()
	if stats.WordCount != 13504 {
		t.Errorf("WordCount = %d, want 13504", stats.WordCount)
	}
	if stats.TransformCount != 121 {
		t.Errorf("TransformCount = %d, want 121", stats.TransformCount)
	}
	if stats.CandidateCount <= 1_500_000 {
		t.Errorf("CandidateCount = %d, expected over 1.5M", stats.CandidateCount)
	}
	if stats.BucketCount == 0 || stats.BucketCount > stats.CandidateCount {
		t.Errorf("BucketCount = %d out of range", stats.BucketCount)
	}
	if stats.ApproxBytes <= 0 {
		t.Errorf("ApproxBytes = %d, want positive", stats.ApproxBytes)
	}
	t.Logf("index stats: %+v", stats)
}
