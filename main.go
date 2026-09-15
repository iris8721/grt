package main

import (
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"time"
)

const (
	baseURL = "https://webapps.regionofwaterloo.ca/api/grt-routes/api"

	busVehiclePositionsURL = baseURL + "/vehiclepositions/1"
	busTripUpdatesURL      = baseURL + "/tripupdates/1"
	lrtVehiclePositionsURL = baseURL + "/vehiclepositions/2"
	lrtTripUpdatesURL      = baseURL + "/tripupdates/2"
	alertsURL              = baseURL + "/alerts"
	busStaticURL           = baseURL + "/staticfeeds/1"
	lrtStaticURL           = baseURL + "/staticfeeds/2"

	gtfsDir            = "data/GTFS"
	pollInterval       = 30 * time.Second
	gtfsUpdateInterval = 24 * time.Hour
	httpPort           = ":8080"
)

func main() {
	app := newApp()

	log.Println("Downloading GTFS static data...")
	if err := downloadAndUpdateGTFS(); err != nil {
		log.Printf("GTFS download failed, falling back to cached data: %v", err)
	}

	log.Println("Loading static GTFS data...")
	sd, err := loadStatic()
	if err != nil {
		log.Fatalf("load static: %v", err)
	}
	log.Printf("Loaded %d stops, %d routes, %d trips", len(sd.StopNames), len(sd.RouteNames), len(sd.TripHeadsigns))

	shapesJSON, err := buildShapesGeoJSON(sd)
	if err != nil {
		log.Printf("shapes: %v", err)
	}

	app.setStatic(sd, shapesJSON)

	go func() {
		log.Printf("Map server running at http://localhost%s", httpPort)
		if err := startServer(app); err != nil {
			log.Fatalf("server: %v", err)
		}
	}()

	time.Sleep(200 * time.Millisecond)
	openBrowser(fmt.Sprintf("http://localhost%s", httpPort))

	go func() {
		ticker := time.NewTicker(gtfsUpdateInterval)
		defer ticker.Stop()
		for range ticker.C {
			log.Println("[GTFS] Downloading updated static feed...")
			if err := downloadAndUpdateGTFS(); err != nil {
				log.Printf("[GTFS] Download failed: %v", err)
				continue
			}
			newSD, err := loadStatic()
			if err != nil {
				log.Printf("[GTFS] Reload failed: %v", err)
				continue
			}
			newShapes, err := buildShapesGeoJSON(newSD)
			if err != nil {
				log.Printf("[GTFS] Shapes error: %v", err)
			}
			app.setStatic(newSD, newShapes)
			log.Printf("[GTFS] Reloaded: %d stops, %d routes, %d trips",
				len(newSD.StopNames), len(newSD.RouteNames), len(newSD.TripHeadsigns))
		}
	}()

	poll := func() {
		snap := fetchAll()
		sd, updatedAt := app.getStatic()
		msg := buildMessage(snap, sd, updatedAt)
		app.hub.broadcast(msg)
	}

	poll()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		poll()
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("could not open browser: %v", err)
	}
}
