package httpx

import (
	"encoding/json"
	"io"
)

func newStrictDecoder(r io.Reader) *json.Decoder {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	return dec
}
