package api

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// backup streams a gzipped tar of loop.db (as `VACUUM INTO` reads it) and the evidence and
// regression files beside it: the nightly copy off the machine, which a volume snapshot is not
// (docs/decisions/2026-10-23-loopd-on-fly.md). scripts/loopd-backup is what calls it.
//
// It is the user's token only, and deliberately not in reads: this one route hands over every
// item, every event and every uploaded file at once, so a runner's token and the orchestrator's
// must not reach it.
func (s *server) backup(w http.ResponseWriter, r *http.Request) {
	name := fmt.Sprintf("loop-backup-%s.tar.gz", time.Now().UTC().Format("20060102T150405Z"))
	// The headers go on with the archive's first byte, which is after Store.Backup has its
	// snapshot: a failure before that is still a status, with no misleading Content-Disposition
	// on it.
	body := &lazyHeader{w: w, set: func(h http.Header) {
		h.Set("Content-Type", "application/gzip")
		h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
	}}
	if err := s.st.Backup(r.Context(), body); err != nil {
		if body.wrote {
			// The body has begun, so the status is already 200 and there is nowhere to say this.
			// The archive ends mid-file, which is why the reader checks it before keeping it.
			s.log.Error("backup failed mid-stream", "err", err)
			return
		}
		s.fail(w, err)
	}
}

// lazyHeader sets the response's headers on the first byte written, and not before.
type lazyHeader struct {
	w     http.ResponseWriter
	set   func(http.Header)
	wrote bool
}

func (l *lazyHeader) Write(p []byte) (int, error) {
	if !l.wrote {
		l.wrote = true
		l.set(l.w.Header())
	}
	return l.w.Write(p)
}

var _ io.Writer = (*lazyHeader)(nil)
