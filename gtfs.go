package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
)

var routePalette = []string{
	"#e74c3c", "#3498db", "#2ecc71", "#f39c12", "#9b59b6",
	"#1abc9c", "#e67e22", "#e91e63", "#ff5722", "#607d8b",
	"#795548", "#8bc34a", "#ff9800", "#673ab7", "#009688",
	"#0097a7", "#558b2f", "#6d4c41", "#d32f2f", "#1565c0",
}

func assignColors(routeIDs []string) map[string]string {
	sorted := make([]string, len(routeIDs))
	copy(sorted, routeIDs)
	sort.Strings(sorted)

	colors := make(map[string]string, len(sorted))
	i := 0
	for _, id := range sorted {
		switch id {
		case "301":
			colors[id] = "#00cccc"
		case "302":
			colors[id] = "#00999a"
		default:
			colors[id] = routePalette[i%len(routePalette)]
			i++
		}
	}
	return colors
}

type StaticData struct {
	StopNames     map[string]string
	RouteNames    map[string]string
	TripHeadsigns map[string]string
	RouteColors   map[string]string
}

func (sd *StaticData) routeName(id string) string {
	if v := sd.RouteNames[id]; v != "" {
		return v
	}
	return "Route " + id
}

func (sd *StaticData) headsign(tripID string) string {
	if v := sd.TripHeadsigns[tripID]; v != "" {
		return v
	}
	return "(unknown)"
}

func (sd *StaticData) stopName(stopID string) string {
	if v := sd.StopNames[stopID]; v != "" {
		return v
	}
	return "Stop " + stopID
}

func (sd *StaticData) color(routeID string) string {
	if v := sd.RouteColors[routeID]; v != "" {
		return v
	}
	return "#888888"
}

func loadCSV(path string, keyCol, valCol int) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.Read()

	m := make(map[string]string)
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if keyCol < len(row) && valCol < len(row) {
			m[row[keyCol]] = row[valCol]
		}
	}
	return m, nil
}

func loadStatic() (*StaticData, error) {
	stops, err := loadCSV(gtfsDir+"/stops.txt", 0, 2)
	if err != nil {
		return nil, fmt.Errorf("stops: %w", err)
	}

	routesFile, err := os.Open(gtfsDir + "/routes.txt")
	if err != nil {
		return nil, fmt.Errorf("routes: %w", err)
	}
	defer routesFile.Close()
	rr := csv.NewReader(routesFile)
	rr.FieldsPerRecord = -1
	rr.Read()
	routes := make(map[string]string)
	for {
		row, err := rr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(row) >= 4 {
			routes[row[0]] = fmt.Sprintf("%s – %s", row[2], row[3])
		}
	}

	headsigns, err := loadCSV(gtfsDir+"/trips.txt", 2, 3)
	if err != nil {
		return nil, fmt.Errorf("trips: %w", err)
	}

	routeIDs := make([]string, 0, len(routes))
	for id := range routes {
		routeIDs = append(routeIDs, id)
	}

	return &StaticData{
		StopNames:     stops,
		RouteNames:    routes,
		TripHeadsigns: headsigns,
		RouteColors:   assignColors(routeIDs),
	}, nil
}

type shapePoint struct {
	lat, lon float64
	seq      int
}

type geoFeature struct {
	Type       string            `json:"type"`
	Properties map[string]string `json:"properties"`
	Geometry   geoLineString     `json:"geometry"`
}

type geoLineString struct {
	Type        string       `json:"type"`
	Coordinates [][2]float64 `json:"coordinates"`
}

type geoCollection struct {
	Type     string       `json:"type"`
	Features []geoFeature `json:"features"`
}

func buildShapesGeoJSON(sd *StaticData) ([]byte, error) {
	type dirKey struct{ routeID, dir string }
	shapeCounts := make(map[dirKey]map[string]int)

	tf, err := os.Open(gtfsDir + "/trips.txt")
	if err != nil {
		return nil, fmt.Errorf("trips: %w", err)
	}
	tr := csv.NewReader(tf)
	tr.FieldsPerRecord = -1
	tr.Read()
	for {
		row, err := tr.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(row) < 7 || row[6] == "" {
			continue
		}
		k := dirKey{row[0], row[4]}
		if shapeCounts[k] == nil {
			shapeCounts[k] = make(map[string]int)
		}
		shapeCounts[k][row[6]]++
	}
	tf.Close()

	canonical := make(map[string]dirKey)
	for k, counts := range shapeCounts {
		best, bestN := "", 0
		for sid, n := range counts {
			if n > bestN {
				best, bestN = sid, n
			}
		}
		canonical[best] = k
	}

	sf, err := os.Open(gtfsDir + "/shapes.txt")
	if err != nil {
		return nil, fmt.Errorf("shapes: %w", err)
	}
	defer sf.Close()

	sr := csv.NewReader(sf)
	sr.FieldsPerRecord = -1
	sr.Read()

	shapePoints := make(map[string][]shapePoint)
	for {
		row, err := sr.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(row) < 4 {
			continue
		}
		sid := row[0]
		if _, needed := canonical[sid]; !needed {
			continue
		}
		lat, err1 := strconv.ParseFloat(row[1], 64)
		lon, err2 := strconv.ParseFloat(row[2], 64)
		seq, err3 := strconv.Atoi(row[3])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		shapePoints[sid] = append(shapePoints[sid], shapePoint{lat, lon, seq})
	}

	for sid := range shapePoints {
		pts := shapePoints[sid]
		sort.Slice(pts, func(i, j int) bool { return pts[i].seq < pts[j].seq })
		shapePoints[sid] = pts
	}

	col := geoCollection{Type: "FeatureCollection"}
	for sid, dk := range canonical {
		pts, ok := shapePoints[sid]
		if !ok || len(pts) < 2 {
			continue
		}
		coords := make([][2]float64, len(pts))
		for i, p := range pts {
			coords[i] = [2]float64{p.lon, p.lat}
		}
		col.Features = append(col.Features, geoFeature{
			Type: "Feature",
			Properties: map[string]string{
				"route_id":   dk.routeID,
				"route_name": sd.routeName(dk.routeID),
				"color":      sd.color(dk.routeID),
				"direction":  dk.dir,
			},
			Geometry: geoLineString{
				Type:        "LineString",
				Coordinates: coords,
			},
		})
	}

	return json.Marshal(col)
}

func downloadZip(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func extractZip(data []byte) (map[string][]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(r.File))
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", f.Name, err)
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f.Name, err)
		}
		files[f.Name] = content
	}
	return files, nil
}

func mergeCSV(busData, lrtData []byte) []byte {
	idx := bytes.IndexByte(lrtData, '\n')
	if idx < 0 {
		return busData
	}
	out := busData
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, lrtData[idx+1:]...)
}

func downloadAndUpdateGTFS() error {
	type zipResult struct {
		label string
		data  []byte
		err   error
	}
	ch := make(chan zipResult, 2)
	for _, src := range []struct{ label, url string }{
		{"bus", busStaticURL},
		{"lrt", lrtStaticURL},
	} {
		src := src
		go func() {
			data, err := downloadZip(src.url)
			ch <- zipResult{src.label, data, err}
		}()
	}

	zips := make(map[string][]byte)
	for range []int{0, 1} {
		r := <-ch
		if r.err != nil {
			return fmt.Errorf("download %s: %w", r.label, r.err)
		}
		zips[r.label] = r.data
	}

	busFiles, err := extractZip(zips["bus"])
	if err != nil {
		return fmt.Errorf("extract bus: %w", err)
	}
	lrtFiles, err := extractZip(zips["lrt"])
	if err != nil {
		return fmt.Errorf("extract lrt: %w", err)
	}

	merged := make(map[string][]byte)
	for name, data := range busFiles {
		merged[name] = data
	}
	for name, lrtData := range lrtFiles {
		if busData, exists := merged[name]; exists && name != "agency.txt" {
			merged[name] = mergeCSV(busData, lrtData)
		} else if !exists {
			merged[name] = lrtData
		}
	}

	if err := os.MkdirAll(gtfsDir, 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	for name, data := range merged {
		if err := os.WriteFile(gtfsDir+"/"+name, data, 0644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}
