package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/template"
	"time"

	"crypto/md5"
	"encoding/hex"
	"github.com/google/uuid"
	"github.com/tmaxmax/go-sse"
)

type NewClientMessage struct {
	MatchID  string
	ClientID string
}

var (
	//go:embed web
	web         embed.FS
	adjustedWeb fs.FS
)

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
	matchIDCookie, _ := r.Cookie("matchID")
	if matchIDCookie == nil {
		homeHandler(w, r)
		return
	}
	matchIDHashed := getMD5Hash(matchIDCookie.Value)
	action := strings.ToUpper(r.PathValue("action"))
	r.Form["action"] = []string{action}
	messageID, _ := uuid.NewV7()
	e := &sse.Message{
		ID:    sse.ID(messageID.String()),
		Type:  sse.Type(action),
		Retry: time.Duration(1 * time.Second),
	}
	messageString := formToJsonString(r.Form)
	e.AppendData(messageString)
	err := sseHandler.Publish(e, matchIDHashed)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("error"))
	}
	log.Println(fmt.Sprintf("actionHandler data: %s", messageString))
	lastMatchMessage[matchIDHashed] = e
	w.Write([]byte(action))
}

// formToJsonString returns the first value of each form as a json key value
// only support single value for each key
func formToJsonString(form url.Values) string {
	formData := map[string]string{}
	for key, values := range form {
		if len(values) > 0 {
			formData[key] = values[0]
		}
	}

	jsonData, _ := json.Marshal(formData)
	return string(jsonData)
}

func listenerHandler(w http.ResponseWriter, r *http.Request) {
	tmpl, _ := template.ParseFS(adjustedWeb, "listener.html", "scoreboard.html")
	tmpl.Execute(w, nil)
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	tmpl, _ := template.ParseFS(adjustedWeb, "base.html", "sender.html", "scoreboard.html")
	cookie, _ := r.Cookie("matchID")
	if cookie == nil {
		cookie = &http.Cookie{
			Name:     "matchID",
			Value:    uuid.NewString(),
			SameSite: http.SameSiteStrictMode,
			HttpOnly: true,
			// Secure:   true, // TODO: Set this based on environment
		}
		http.SetCookie(w, cookie)
	}

	tmpl.Execute(w, map[string]string{
		"MatchID": getMD5Hash(cookie.Value),
	})
}

func initNewSSEClient(ch <-chan NewClientMessage) {
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

func useMiddleware(h http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	for _, m := range middlewares {
		h = m(h)
	}
	return h
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("Panic: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	adjustedWeb, _ = fs.Sub(web, "web")
	go initNewSSEClient(newClientChan)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	fs := http.FileServer(http.Dir("./web/static"))
	mux.Handle("/static/", http.StripPrefix("/static/", fs))
	mux.HandleFunc("/stop", func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		w.WriteHeader(http.StatusOK)
	})

	mux.Handle("GET /events", sseHandler)

	mux.HandleFunc("/", homeHandler)
	mux.HandleFunc("POST /api/match/{action}", actionHandler)
	mux.HandleFunc("POST /api/send", sendHandler)
	mux.HandleFunc("GET /listen", listenerHandler)

	s := &http.Server{
		Addr:              ":8080",
		ReadHeaderTimeout: time.Second * 1,
		Handler:           useMiddleware(mux, recoveryMiddleware),
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

func getMD5Hash(text string) string {
	hash := md5.Sum([]byte(text))
	return hex.EncodeToString(hash[:])
}
