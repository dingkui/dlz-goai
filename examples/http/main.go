// An embeddable HTTP adapter with a deterministic model; no API key is needed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	dlzgoai "github.com/dingkui/dlz-goai"
	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/runtime"
	"github.com/dingkui/dlz-goai/storage/sqlite"
	"github.com/dingkui/dlz-goai/tool"
)

func demoRequest() dlzgoai.Request {
	return dlzgoai.Request{Model: func(ctx context.Context, msgs []message.Message, _ *message.Options, emit func(message.Delta)) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if msgs[len(msgs)-1].Role == message.RoleTool {
			emit(message.Delta{Content: "Finished: " + msgs[len(msgs)-1].Content})
			return nil
		}
		emit(message.Delta{ToolCalls: []tool.CallDelta{{Index: 0, ID: "demo-call", Name: "demo_action", Arguments: "{}"}}})
		return nil
	}, Tools: []tool.Tool{tool.NewFunc("demo_action", "Approval demo (no external changes)", nil, false, func(context.Context, map[string]any) (tool.Result, error) {
		return tool.Text("approved demo action"), nil
	})}}
}

func newHandler(client *dlzgoai.Client) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, err error) {
		status := http.StatusBadRequest
		if errors.Is(err, runtime.ErrRunActive) || errors.Is(err, runtime.ErrRunTerminal) {
			status = http.StatusConflict
		}
		if errors.Is(err, runtime.ErrRunNotFound) {
			status = http.StatusNotFound
		}
		reply(w, status, map[string]string{"error": err.Error()})
	}
	mux.HandleFunc("POST /runs", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input string `json:"input"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		req := demoRequest()
		req.Input = body.Input
		run, err := client.Start(r.Context(), req)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, http.StatusAccepted, map[string]string{"run_id": run.ID()})
	})
	mux.HandleFunc("GET /runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		record, err := client.GetRun(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, record)
	})
	mux.HandleFunc("POST /runs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if !client.Cancel(r.PathValue("id")) {
			fail(w, runtime.ErrRunNotFound)
			return
		}
		reply(w, 202, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /runs/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		run, err := client.ResumeWith(r.Context(), r.PathValue("id"), demoRequest())
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 202, map[string]string{"run_id": run.ID()})
	})
	mux.HandleFunc("POST /runs/{id}/approvals/{call}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Approved *bool `json:"approved"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		if body.Approved == nil {
			fail(w, errors.New("approved is required"))
			return
		}
		if err := client.Approve(r.PathValue("id"), r.PathValue("call"), *body.Approved); err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /runs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := client.GetRun(r.Context(), id); err != nil {
			fail(w, err)
			return
		}
		cursor := r.URL.Query().Get("after")
		if cursor == "" {
			cursor = r.Header.Get("Last-Event-ID")
		}
		var after int64
		if cursor != "" {
			var err error
			after, err = strconv.ParseInt(cursor, 10, 64)
			if err != nil || after < 0 {
				fail(w, errors.New("invalid event cursor"))
				return
			}
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		rc := http.NewResponseController(w)
		_ = client.Stream(ctx, id, after, func(e agent.Event) {
			data, err := json.Marshal(e)
			if err != nil {
				cancel()
				return
			}
			if _, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Seq, data); err != nil {
				cancel()
				return
			}
			if err = rc.Flush(); err != nil {
				cancel()
			}
		})
	})
	return mux
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8088", "HTTP listen address")
	path := flag.String("db", "http-demo.db", "SQLite database path")
	flag.Parse()
	db, err := sqlite.Open(*path)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	rt := runtime.New(runtime.Options{Runs: sqlite.NewRunStore(db), Events: sqlite.NewEventStore(db), Checkpoints: sqlite.NewCheckpointStore(db)})
	if _, err = rt.Recover(context.Background()); err != nil {
		log.Fatal(err)
	}
	client, err := dlzgoai.NewClient(dlzgoai.Options{Runtime: rt})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()
	server := &http.Server{Addr: *addr, Handler: newHandler(client), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		client.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("HTTP demo: http://%s", *addr)
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Print(err)
	}
}
