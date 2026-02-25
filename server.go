package main

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"sync"
	"time"
)

//go:embed static
var staticFiles embed.FS

type App struct {
	mu            sync.RWMutex
	sd            *StaticData
	shapesJSON    []byte
	gtfsUpdatedAt time.Time
	hub           *Hub
}

func newApp() *App {
	return &App{hub: newHub()}
}

func (a *App) setStatic(sd *StaticData, shapesJSON []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sd = sd
	a.shapesJSON = shapesJSON
	a.gtfsUpdatedAt = time.Now()
}

func (a *App) getStatic() (*StaticData, time.Time) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.sd, a.gtfsUpdatedAt
}

type Hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	lastMsg []byte
}

func newHub() *Hub {
	return &Hub{clients: make(map[chan []byte]struct{})}
}

func (h *Hub) addClient() chan []byte {
	ch := make(chan []byte, 4)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) removeClient(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	close(ch)
	h.mu.Unlock()
}

func (h *Hub) broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastMsg = msg
	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
		}
	}
}

func startServer(app *App) error {
	mux := http.NewServeMux()

	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	mux.HandleFunc("/api/shapes", func(w http.ResponseWriter, r *http.Request) {
		app.mu.RLock()
		data := app.shapesJSON
		app.mu.RUnlock()

		if data == nil {
			http.Error(w, `{"type":"FeatureCollection","features":[]}`, http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})

	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "SSE not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		ch := app.hub.addClient()
		defer app.hub.removeClient(ch)

		app.hub.mu.Lock()
		last := app.hub.lastMsg
		app.hub.mu.Unlock()
		if last != nil {
			fmt.Fprintf(w, "data: %s\n\n", last)
			flusher.Flush()
		}

		for {
			select {
			case msg, ok := <-ch:
				if !ok {
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", msg)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	return http.ListenAndServe(httpPort, mux)
}
