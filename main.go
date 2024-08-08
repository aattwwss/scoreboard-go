package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/tmaxmax/go-sse"
)

var masterSSEHandler = &sse.Server{}
var sseHandler = &sse.Server{
	Provider: &sse.Joe{
		ReplayProvider: &sse.ValidReplayProvider{
			TTL: time.Minute * 5,
		},
	},
	OnSession: func(s *sse.Session) (sse.Subscription, bool) {
		s.Req.ParseForm()
		matchID := s.Req.FormValue("matchID")
		clientID := s.Req.FormValue("clientID")
		log.Println(fmt.Sprintf("Connected with last event id: %s, matchID: %s, clientID: %s", s.LastEventID, matchID, clientID))
		return sse.Subscription{
			LastEventID: s.LastEventID,
			Client:      s,
			Topics:      []string{matchID, clientID},
		}, true
	},
}

type TimerState struct {
	MessageCreateTime int    `json:"messageCreateTime"`
	StartTime         int    `json:"startTime"`
	TimerValue        string `json:"timerValue"`
	Action            string `json:"action"`
}

func sendHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	msg := r.FormValue("message")
	topic := r.FormValue("topic")
	e := &sse.Message{}
	e.AppendData(msg)
	e.ID = sse.ID(time.Now().String())
	err := sseHandler.Publish(e, topic)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("error"))
	}
	w.Write([]byte("OK"))
}

func startHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	msg := r.FormValue("time")
	createTimeParam := r.FormValue("messageCreateTime")
	matchID := r.FormValue("matchID")
	createTime, _ := strconv.Atoi(createTimeParam)
	startTime, _ := strconv.Atoi(r.FormValue("startTime"))
	state := TimerState{
		MessageCreateTime: createTime,
		StartTime:         startTime,
		TimerValue:        msg,
		Action:            "START",
	}
	json, _ := json.Marshal(state)
	e := &sse.Message{
		ID:    sse.ID(createTimeParam),
		Type:  sse.Type("START"),
		Retry: time.Duration(1 * time.Second),
	}
	e.AppendData(string(json))
	err := sseHandler.Publish(e, matchID)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("error"))
	}
	w.Write([]byte("STARTED"))
}

func stopHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	msg := r.FormValue("time")
	createTimeParam := r.FormValue("messageCreateTime")
	matchID := r.FormValue("matchID")
	createTime, _ := strconv.Atoi(createTimeParam)
	startTime, _ := strconv.Atoi(r.FormValue("startTime"))
	state := TimerState{
		MessageCreateTime: createTime,
		StartTime:         startTime,
		TimerValue:        msg,
		Action:            "STOP",
	}
	json, _ := json.Marshal(state)
	e := &sse.Message{
		ID:    sse.ID(createTimeParam),
		Type:  sse.Type("STOP"),
		Retry: time.Duration(10 * time.Second),
	}
	e.AppendData(string(json))
	err := sseHandler.Publish(e, matchID)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("error"))
	}
	w.Write([]byte("STOPPED"))
}

func resetHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	msg := r.FormValue("time")
	createTimeParam := r.FormValue("messageCreateTime")
	matchID := r.FormValue("matchID")
	createTime, _ := strconv.Atoi(createTimeParam)
	startTime, _ := strconv.Atoi(r.FormValue("startTime"))
	state := TimerState{
		MessageCreateTime: createTime,
		StartTime:         startTime,
		TimerValue:        msg,
		Action:            "RESET",
	}
	json, _ := json.Marshal(state)
	e := &sse.Message{
		ID:    sse.ID(createTimeParam),
		Type:  sse.Type("RESET"),
		Retry: time.Duration(10 * time.Second),
	}
	e.AppendData(string(json))
	err := sseHandler.Publish(e, matchID)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("error"))
	}
	w.Write([]byte("RESETED"))
}

func listenerHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/listener.html")
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/sender.html")
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	mux := http.NewServeMux()
	mux.HandleFunc("/stop", func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		w.WriteHeader(http.StatusOK)
	})

	mux.Handle("GET /events", sseHandler)
	mux.Handle("GET /master/events", masterSSEHandler)

	mux.HandleFunc("/", homeHandler)
	mux.HandleFunc("/listen", listenerHandler)
	mux.HandleFunc("POST /send", sendHandler)
	mux.HandleFunc("POST /start", startHandler)
	mux.HandleFunc("POST /stop", stopHandler)
	mux.HandleFunc("POST /reset", resetHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	s := &http.Server{
		Addr:              "0.0.0.0:8080",
		ReadHeaderTimeout: time.Second * 10,
		Handler:           mux,
	}
	s.RegisterOnShutdown(func() {
		e := &sse.Message{Type: sse.Type("close")}
		// Adding data is necessary because spec-compliant clients
		// do not dispatch events without data.
		e.AppendData("bye")
		// Broadcast a close message so clients can gracefully disconnect.
		_ = sseHandler.Publish(e)

		ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()

		// We use a context with a timeout so the program doesn't wait indefinitely
		// for connections to terminate. There may be misbehaving connections
		// which may hang for an unknown timespan, so we just stop waiting on Shutdown
		// after a certain duration.
		_ = sseHandler.Shutdown(ctx)
	})

	if err := runServer(ctx, s); err != nil {
		log.Println("server closed", err)
	}
}

func runServer(ctx context.Context, s *http.Server) error {
	log.Printf("running server")
	shutdownError := make(chan error)

	go func() {
		<-ctx.Done()

		sctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
		defer cancel()

		shutdownError <- s.Shutdown(sctx)
	}()

	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return <-shutdownError
}
