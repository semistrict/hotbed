package worker

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/httpx"
)

// Handler serves the engine to the controller, which proxies clients' calls
// about workspaces and commands on this node here.
func (e *Engine) Handler() http.Handler {
	mux := http.NewServeMux()
	h := httpx.Handle
	mux.HandleFunc("POST /v1/workspaces/{id}", h(func(w http.ResponseWriter, r *http.Request) error {
		var req api.CreateWorkspace
		if err := httpx.Decode(r, &req); err != nil {
			return err
		}
		ws, err := e.CreateWorkspace(r.Context(), r.PathValue("id"), req)
		if err != nil {
			return err
		}
		return httpx.JSON(w, ws)
	}))
	mux.HandleFunc("GET /v1/workspaces/{id}", h(func(w http.ResponseWriter, r *http.Request) error {
		ws, err := e.Workspace(r.PathValue("id"))
		if err != nil {
			return err
		}
		return httpx.JSON(w, ws)
	}))
	mux.HandleFunc("DELETE /v1/workspaces/{id}", h(func(w http.ResponseWriter, r *http.Request) error {
		if err := e.ReleaseWorkspace(r.Context(), r.PathValue("id")); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
	mux.HandleFunc("POST /v1/workspaces/{id}/sync", h(func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query()
		res, err := e.Sync(r.Context(), r.PathValue("id"), r.Body, q.Get("commit"), q.Get("tree"))
		if err != nil {
			return err
		}
		return httpx.JSON(w, res)
	}))
	mux.HandleFunc("GET /v1/workspaces/{id}/files", h(func(w http.ResponseWriter, r *http.Request) error {
		files, err := e.Files(r.Context(), r.PathValue("id"))
		if err != nil {
			return err
		}
		return httpx.JSON(w, files)
	}))
	mux.HandleFunc("GET /v1/workspaces/{id}/files/{path...}", h(func(w http.ResponseWriter, r *http.Request) error {
		f, err := e.File(r.Context(), r.PathValue("id"), r.PathValue("path"))
		if err != nil {
			return err
		}
		return httpx.JSON(w, f)
	}))
	mux.HandleFunc("POST /v1/workspaces/{id}/commands/{cid}", h(func(w http.ResponseWriter, r *http.Request) error {
		var req api.RunCommand
		if err := httpx.Decode(r, &req); err != nil {
			return err
		}
		c, err := e.Exec(r.Context(), r.PathValue("id"), r.PathValue("cid"), req)
		if err != nil {
			return err
		}
		return httpx.JSON(w, c)
	}))
	mux.HandleFunc("GET /v1/commands/{id}", h(func(w http.ResponseWriter, r *http.Request) error {
		c, ok := e.Command(r.PathValue("id"))
		if !ok {
			return api.Errorf(api.CodeNotFound, "no command %s on this node", r.PathValue("id"))
		}
		return httpx.JSON(w, c)
	}))
	mux.HandleFunc("POST /v1/commands/{id}/stop", h(func(w http.ResponseWriter, r *http.Request) error {
		if err := e.Stop(r.Context(), r.PathValue("id"), "stopped by "+httpx.Peer(r.Context()).Hostname); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
	mux.HandleFunc("GET /v1/commands/{id}/log", h(e.serveLog))
	mux.HandleFunc("GET /v1/commands/{id}/lines", h(func(w http.ResponseWriter, r *http.Request) error {
		path, done, err := e.Follow(r.PathValue("id"))
		if err != nil {
			return err
		}
		done()
		tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
		if tail <= 0 {
			tail = 2000
		}
		lines, err := tailLines(path, tail)
		if err != nil {
			return err
		}
		return httpx.JSON(w, api.CommandLog{CommandID: r.PathValue("id"), Lines: lines})
	}))
	return mux
}

// serveLog streams a command's output from offset. With follow=1 it keeps
// the stream open until the command ends, which is how an attached exec
// shows output and keeps its command from counting as orphaned.
func (e *Engine) serveLog(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	path, done, err := e.Follow(id)
	if err != nil {
		return err
	}
	defer done()
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	follow := r.URL.Query().Get("follow") == "1"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 64<<10)
	for {
		f, err := os.Open(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// Queued: no output yet.
		case err != nil:
			return err
		default:
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				f.Close()
				return err
			}
			for {
				n, rerr := f.Read(buf)
				if n > 0 {
					if _, err := w.Write(buf[:n]); err != nil {
						f.Close()
						return nil // the client went away
					}
					offset += int64(n)
				}
				if rerr != nil {
					break
				}
			}
			f.Close()
		}
		if flusher != nil {
			flusher.Flush()
		}
		if !follow || !e.Live(id) {
			// One more read catches output written between the last read
			// and the command ending.
			if follow {
				follow = false
				continue
			}
			return nil
		}
		select {
		case <-r.Context().Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// tailLines returns the last n lines of a file.
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		lines = append(lines, strings.TrimRight(sc.Text(), "\r"))
		if len(lines) > 2*n {
			lines = append(lines[:0], lines[len(lines)-n:]...)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if lines == nil {
		lines = []string{}
	}
	return lines, sc.Err()
}
