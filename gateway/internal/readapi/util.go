package readapi

import (
	"bytes"
	"io"
)

func jsonReader(b []byte) io.Reader { return bytes.NewReader(b) }
