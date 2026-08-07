package main

import (
	"bufio"
	"encoding/json"
	"io"

	"strings"

	"onetoken/internal/store"
)

func decodeJSONL(r io.Reader) ([]*store.Response, error) {
	var out []*store.Response
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var resp store.Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			return nil, err
		}
		out = append(out, &resp)
	}
	return out, sc.Err()
}
