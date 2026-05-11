package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	gtfs "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

type Vehicle struct {
	ID        string  `json:"id"`
	Route     string  `json:"route,omitempty"`
	Trip      string  `json:"trip,omitempty"`
	Lat       float64 `json:"lat"`
	Lon       float64 `json:"lon"`
	Bearing   float32 `json:"bearing,omitempty"`
	Speed     float32 `json:"speed,omitempty"`
	Timestamp int64   `json:"ts"`
}

type store struct {
	mu       sync.RWMutex
	vehicles map[string]Vehicle
}

func newStore() *store {
	return &store{vehicles: make(map[string]Vehicle)}
}

func (s *store) snapshot() []Vehicle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Vehicle, 0, len(s.vehicles))
	for _, v := range s.vehicles {
		out = append(out, v)
	}
	return out
}

func (s *store) replace(vs []Vehicle) {
	next := make(map[string]Vehicle, len(vs))
	for _, v := range vs {
		next[v.ID] = v
	}
	s.mu.Lock()
	s.vehicles = next
	s.mu.Unlock()
}

func poll(ctx context.Context, s *store, apiKey, agency string) {
	url := fmt.Sprintf("https://api.511.org/transit/vehiclepositions?api_key=%s&agency=%s", apiKey, agency)
	client := &http.Client{Timeout: 10 * time.Second}

	pollOnce := func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			log.Printf("poll: request: %v", err)
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("poll: fetch: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("poll: status %d", resp.StatusCode)
			return
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("poll: read: %v", err)
			return
		}
		feed := &gtfs.FeedMessage{}
		if err := proto.Unmarshal(body, feed); err != nil {
			log.Printf("poll: unmarshal: %v", err)
			return
		}
		out := make([]Vehicle, 0, len(feed.Entity))
		for _, e := range feed.Entity {
			vp := e.GetVehicle()
			if vp == nil || vp.Position == nil {
				continue
			}
			v := Vehicle{
				ID:        e.GetId(),
				Lat:       float64(vp.Position.GetLatitude()),
				Lon:       float64(vp.Position.GetLongitude()),
				Bearing:   vp.Position.GetBearing(),
				Speed:     vp.Position.GetSpeed(),
				Timestamp: int64(vp.GetTimestamp()),
			}
			if t := vp.GetTrip(); t != nil {
				v.Route = t.GetRouteId()
				v.Trip = t.GetTripId()
			}
			out = append(out, v)
		}
		s.replace(out)
		log.Printf("poll: %s -> %d vehicles", agency, len(out))
	}

	pollOnce()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pollOnce()
		}
	}
}

func writeSSE(w http.ResponseWriter, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func main() {
	apiKey := os.Getenv("TRANSIT_API_KEY")
	if apiKey == "" {
		log.Fatal("TRANSIT_API_KEY env var required (get one at https://511.org/open-data/token)")
	}
	agency := os.Getenv("TRANSIT_AGENCY")
	if agency == "" {
		agency = "SF"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	s := newStore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poll(ctx, s, apiKey, agency)

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/api/vehicles", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(s.snapshot())
	})

	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		if err := writeSSE(w, s.snapshot()); err != nil {
			return
		}
		flusher.Flush()

		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if err := writeSSE(w, s.snapshot()); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})

	mux.Handle("/", http.FileServer(http.Dir("./web/build")))

	log.Printf("transit-ops listening on :%s (agency=%s)", port, agency)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
