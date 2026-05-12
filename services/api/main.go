package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
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
	Mode      string  `json:"mode,omitempty"`
}

type Agency struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

var availableAgencies = []Agency{
	{Code: "SF", Name: "SF Muni"},
	{Code: "BA", Name: "BART"},
	{Code: "CT", Name: "Caltrain"},
	{Code: "AC", Name: "AC Transit"},
	{Code: "SC", Name: "VTA (Santa Clara)"},
	{Code: "GG", Name: "Golden Gate Transit"},
	{Code: "SM", Name: "SamTrans"},
	{Code: "RG", Name: "Regional (all operators)"},
}

func isAgencyValid(code string) bool {
	for _, a := range availableAgencies {
		if a.Code == code {
			return true
		}
	}
	return false
}

// modeFor returns a human-readable mode string for a given (agency, route).
// Route-naming conventions are agency-specific.
func modeFor(agency, route string) string {
	if route == "" {
		return "Non-revenue / between runs"
	}
	switch agency {
	case "SF":
		// Numeric → bus
		if route[0] >= '0' && route[0] <= '9' {
			return "Bus"
		}
		// Single letter rail lines
		if len(route) == 1 && strings.Contains("TNMLKJ", route) {
			return "Light rail"
		}
		switch route {
		case "F":
			return "Historic streetcar"
		case "FBUS":
			return "F-line replacement bus"
		case "PM", "PH", "CA":
			return "Cable car"
		}
		return "Other"
	case "BA":
		return "Heavy rail (BART)"
	case "CT":
		return "Commuter rail"
	case "AC", "GG", "SM":
		return "Bus"
	case "SC":
		if route[0] >= '0' && route[0] <= '9' {
			return "Bus"
		}
		return "Light rail"
	case "RG":
		return "Regional vehicle"
	}
	return "Vehicle"
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

// Mutable agency target — protected by RWMutex, signalled via pollTrigger.
var (
	agencyMu      sync.RWMutex
	currentAgency string
	pollTrigger   = make(chan struct{}, 1)
)

func getAgency() string {
	agencyMu.RLock()
	defer agencyMu.RUnlock()
	return currentAgency
}

func setAgency(s *store, code string) {
	agencyMu.Lock()
	currentAgency = code
	agencyMu.Unlock()
	s.replace(nil) // clear old vehicles immediately
	select {
	case pollTrigger <- struct{}{}:
	default:
	}
}

func pollOnce(ctx context.Context, client *http.Client, s *store, apiKey string) {
	agency := getAgency()
	url := fmt.Sprintf("https://api.511.org/transit/vehiclepositions?api_key=%s&agency=%s", apiKey, agency)
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
		log.Printf("poll: %s status %d", agency, resp.StatusCode)
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
		v.Mode = modeFor(agency, v.Route)
		out = append(out, v)
	}
	s.replace(out)
	log.Printf("poll: %s -> %d vehicles", agency, len(out))
}

func poll(ctx context.Context, s *store, apiKey string) {
	client := &http.Client{Timeout: 10 * time.Second}
	pollOnce(ctx, client, s, apiKey)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pollOnce(ctx, client, s, apiKey)
		case <-pollTrigger:
			pollOnce(ctx, client, s, apiKey)
			ticker.Reset(30 * time.Second)
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
	startup := os.Getenv("TRANSIT_AGENCY")
	if startup == "" {
		startup = "SF"
	}
	if !isAgencyValid(startup) {
		log.Printf("warning: TRANSIT_AGENCY=%q is not in availableAgencies, accepting anyway", startup)
	}
	currentAgency = startup
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	s := newStore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poll(ctx, s, apiKey)

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

	mux.HandleFunc("/api/agency", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"agency":    getAgency(),
				"available": availableAgencies,
			})
		case http.MethodPost:
			var req struct {
				Agency string `json:"agency"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid body", http.StatusBadRequest)
				return
			}
			if !isAgencyValid(req.Agency) {
				http.Error(w, "unknown agency", http.StatusBadRequest)
				return
			}
			setAgency(s, req.Agency)
			log.Printf("agency switched to %s", req.Agency)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.Handle("/", http.FileServer(http.Dir("./web/build")))

	log.Printf("transit-ops listening on :%s (startup agency=%s)", port, startup)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
