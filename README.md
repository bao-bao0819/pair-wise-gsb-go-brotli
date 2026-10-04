This package is a brotli compressor and decompressor implemented in Go.
It was translated from the reference implementation (https://github.com/google/brotli)
with the `c2go` tool at https://github.com/andybalholm/c2go.

I am using it in production with https://github.com/andybalholm/redwood.

API documentation is found at https://pkg.go.dev/github.com/andybalholm/brotli?tab=doc.

## Roadmap

I have been working on new compression algorithms (not translated from C)
in the matchfinder package.
You can use them with the NewWriterV2 function.
Currently they give better results than the old implementation
(at least for compressing my test file, Newton’s *Opticks*) 
on levels 0 to 9.

The new APIs are currently considered experimental,
and are not covered by any SemVer compatibility guarantees.

## Bounded output size

`MaxEncodedSize(srcLen, options, maxFlushes)` returns a guaranteed upper
bound on the encoded size of a payload, so a service can allocate its
output buffer up front instead of estimating from samples. The bound
applies to streams produced by `NewBoundedWriter` with the same
`WriterOptions`, at most `maxFlushes` calls to `Flush`, and a final
`Close`. Unsupported parameters, an unknown flush count, or arithmetic
overflow are reported as errors rather than a plausible-looking number.

`BoundedWriter` is an opt-in encoding mode that makes the bound provable:
it cuts the payload into meta-blocks of at most 2^20 bytes and emits each
block compressed only when that is strictly smaller than the block's legal
uncompressed representation, so local encoding can never exceed the bound
and output is never truncated. The bound itself is derived term by term
from the RFC 7932 container format (stream header, meta-block length/type
fields, uncompressed-block fallback, byte alignment, termination marker);
see the documentation in `bounded.go` for the full formula.
