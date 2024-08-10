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
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/tmaxmax/go-sse"
)

type NewClientMessage struct {
	MatchID  string
	ClientID string
}

var newClientChan = make(chan NewClientMessage)

var lastMatchMessage = map[string]*sse.Message{}

var sseHandler = &sse.Server{
	Provider: &sse.Joe{
		ReplayProvider: &sse.ValidReplayProvider{
			TTL: time.Minute * 5,
		},
	},
	OnSession: func(s *sse.Session) (sse.Subscription, bool) {
		s.Req.ParseForm()
		matchID := s.Req.FormValue("matchID")
		// clientID := s.Req.FormValue("clientID")
		clientID := uuid.New().String()
		log.Println(fmt.Sprintf("Connected with last event id: %s, matchID: %s, clientID: %s", s.LastEventID, matchID, clientID))
		defer func() {
			newClientChan <- NewClientMessage{
				MatchID:  matchID,
				ClientID: clientID,
			}
		}()
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

func actionHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	matchID := r.FormValue("matchID")
	action := strings.ToUpper(r.PathValue("action"))
	formData := map[string]string{
		"action": action,
	}
	for key, values := range r.Form {
		if len(values) > 0 {
			formData[key] = values[0]
		}
	}

	jsonData, _ := json.Marshal(formData)
	messageID, _ := uuid.NewV7()
	e := &sse.Message{
		ID:    sse.ID(messageID.String()),
		Type:  sse.Type(action),
		Retry: time.Duration(1 * time.Second),
	}
	e.AppendData(string(jsonData))
	err := sseHandler.Publish(e, matchID)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("error"))
	}
	log.Println(fmt.Sprintf("actionHandler data: %s", string(jsonData)))
	lastMatchMessage[matchID] = e
	w.Write([]byte(action))
}

func listenerHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/listener.html")
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/sender.html")
}

func initNewClient(ch <-chan NewClientMessage) {
	for message := range ch {
		prevData, ok := lastMatchMessage[message.MatchID]
		if ok {
			messageID, _ := uuid.NewV7()
			prevData.ID = sse.ID(messageID.String())
			log.Println(fmt.Sprintf("Replaying last event for matchID: %s to clientID: %s with data: %v", message.MatchID, message.ClientID, *prevData))
			sseHandler.Publish(prevData, message.ClientID)
		}
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go initNewClient(newClientChan)

	mux := http.NewServeMux()
	mux.HandleFunc("/stop", func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		w.WriteHeader(http.StatusOK)
	})

	mux.Handle("GET /events", sseHandler)

	mux.HandleFunc("/", homeHandler)
	mux.HandleFunc("POST /match/{action}", actionHandler)
	mux.HandleFunc("POST /debug/send", sendHandler)
	mux.HandleFunc("/listen", listenerHandler)

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
