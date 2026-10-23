package engine

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// maxBody bounds a request body; the largest valid submission is well under
// it.
const maxBody = 1 << 20

// NewAPI returns the ingest HTTP handler: POST /v1/messages.
func NewAPI(r *Runner) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, req *http.Request) {
		key, err := idempotencyKey(req.Header)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		var s Submission
		dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body: " + err.Error()})
			return
		}
		if _, err := dec.Token(); err != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body: trailing data after the JSON object"})
			return
		}
		if err := s.Validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		res, err := r.Ingest(req.Context(), &s, key)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		if res.Status == http.StatusConflict {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "idempotency key already used with a different payload", "id": res.MsgID})
			return
		}
		if res.Replay {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"id": res.MsgID})
	})
	return mux
}

// idempotencyKey reads Idempotency-Key: absent means no key; present, it is
// 1 to MaxIdempotencyKey printable ASCII characters [R-RESEND-IDEMP].
func idempotencyKey(h http.Header) (string, error) {
	vs := h.Values("Idempotency-Key")
	if len(vs) == 0 {
		return "", nil
	}
	if len(vs) > 1 {
		return "", errors.New("Idempotency-Key given more than once")
	}
	k := strings.TrimSpace(vs[0])
	if k == "" || len(k) > MaxIdempotencyKey {
		return "", errors.New("Idempotency-Key must be 1-256 characters")
	}
	for i := 0; i < len(k); i++ {
		if c := k[i]; c < 0x20 || c > 0x7e {
			return "", errors.New("Idempotency-Key must be printable ASCII")
		}
	}
	return k, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
