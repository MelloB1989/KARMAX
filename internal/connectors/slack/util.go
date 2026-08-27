package slack

import (
	"io"
	"net/http"
)

func readAll(res *http.Response) ([]byte, error) {
	return io.ReadAll(io.LimitReader(res.Body, 4<<20))
}
