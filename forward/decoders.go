package forward

import "encoding/json"

// Decoder converts a raw line from a [RecordSource] into a value of type T.
// A non-nil error causes the line to be skipped; wrap with [ErrPermanent] to
// abort the Forwarder.
//
// Decoder is invoked synchronously from the [Forwarder.Run] loop and must
// be non-blocking and CPU-bound. The Decoder type intentionally omits a
// context.Context parameter (mirroring [encoding/json.Unmarshal]); a decoder
// that does I/O will hang Run without an escape hatch. If you need network
// or filesystem I/O on a per-record basis, do it in the [Sink] (which does
// accept ctx) or in a goroutine fed by the Sink.
type Decoder[T any] func(line []byte) (T, error)

// IdentityDecoder returns the line as-is without copying. The returned slice
// aliases the LineReader's internal buffer.
//
// UNSAFE INSIDE THE FORWARDER PIPELINE. The Forwarder accumulates decoded
// values into a batch across multiple Source.Next calls before flushing to
// the Sink, so earlier batch entries point at buffer bytes that subsequent
// reads have already overwritten. The Sink will see scrambled or duplicated
// content. Inside the Forwarder, always use [IdentityDecoderCopy].
//
// This function exists for callers driving a [Decoder] outside the Forwarder
// (e.g. a custom pipeline that consumes one record at a time and never
// retains the slice across the next call to Source.Next). It is also used by
// the in-tree benchmarks, which deliberately pair it with a discarding sink
// to measure the zero-copy hot path.
func IdentityDecoder(line []byte) ([]byte, error) {
	return line, nil
}

// IdentityDecoderCopy copies the line into a freshly allocated slice.
// Safe to retain across iterations.
func IdentityDecoderCopy(line []byte) ([]byte, error) {
	cp := make([]byte, len(line))
	copy(cp, line)
	return cp, nil
}

// JSONDecoder returns a [Decoder] that unmarshals each line into a value of
// type T using [encoding/json].
func JSONDecoder[T any]() Decoder[T] {
	return func(line []byte) (T, error) {
		var v T
		if err := json.Unmarshal(line, &v); err != nil {
			return v, err
		}
		return v, nil
	}
}
