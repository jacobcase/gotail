package forward

import "encoding/json"

// Decoder converts a raw line from a [RecordSource] into a value of type T.
// A non-nil error causes the line to be skipped; wrap with [ErrPermanent] to
// abort the Forwarder.
type Decoder[T any] func(line []byte) (T, error)

// IdentityDecoder returns the line as-is without copying. The returned slice
// aliases the LineReader's internal buffer and is only valid until the next
// call to Source.Records. Use [IdentityDecoderCopy] if you need to retain
// values across iterations.
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
