package workers

import (
	"encoding/json"
	"errors"

	"mimamori/app/modules"
)

// errBodyRefused is what the body of a request reads whose large body was
// not read because the request is not authenticated for its route.
var errBodyRefused = errors.New("request body not read: a body this large needs an authenticated request")

// internalErrorBody is the body of the answer to a handler that panicked.
var internalErrorBody = errorBodyBytes(modules.M("common.internal_error"))

// errorBodyBytes returns the JSON body of an error answer (see errorBody).
func errorBodyBytes(m modules.Msg) []byte {
	b, err := json.Marshal(errorBody(m))
	if err != nil {
		panic(err) // a message of strings and numbers always encodes
	}
	return b
}

// errReader returns its error on every read.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
