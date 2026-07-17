// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
)

func handleListen(reg *registry) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		pid, err := strconv.Atoi(r.PathValue("pid"))
		if err != nil {
			http.Error(w, "cannot extract pid", http.StatusBadRequest)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		l, unregister, err := reg.listen(pid)
		if err != nil {
			http.Error(w, "unknown pid", http.StatusNotFound)
			return
		}
		defer unregister() // close the listener when this handler returns

		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher.Flush() // commit 200 + headers so the client sees the stream open

		enc := json.NewEncoder(w)
		ctx := r.Context()
		values := l.recv()
		for {
			select {
			case <-ctx.Done():
				return
			case v := <-values:
				if err := enc.Encode(v); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}
