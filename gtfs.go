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
	"strings"
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

type csvRow struct {
	cols   map[string]int
	fields []string
}

func (row csvRow) get(name string) string {
	i, ok := row.cols[name]
	if !ok || i >= len(row.fields) {
		return ""
	}
	return row.fields[i]
}

func readCSV(path string, required []string, fn func(row csvRow)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return fmt.Errorf("%s: header: %w", path, err)
	}
	cols := make(map[string]int, len(header))
	for i, h := range header {
		cols[strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))] = i
	}
	for _, name := range required {
		if _, ok := cols[name]; !ok {
			return fmt.Errorf("%s: missing column %q", path, name)
		}
	}

	for {
		fields, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		fn(csvRow{cols, fields})
	}
}

func loadCSV(path, keyCol, valCol string) (map[string]string, error) {
	m := make(map[string]string)
	err := readCSV(path, []string{keyCol, valCol}, func(row csvRow) {
		m[row.get(keyCol)] = row.get(valCol)
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

func loadStatic() (*StaticData, error) {
	stops, err := loadCSV(gtfsDir+"/stops.txt", "stop_id", "stop_name")
	if err != nil {
		return nil, fmt.Errorf("stops: %w", err)
	}

	routes := make(map[string]string)
	err = readCSV(gtfsDir+"/routes.txt", []string{"route_id", "route_short_name", "route_long_name"}, func(row csvRow) {
		routes[row.get("route_id")] = fmt.Sprintf("%s – %s", row.get("route_short_name"), row.get("route_long_name"))
	})
	if err != nil {
		return nil, fmt.Errorf("routes: %w", err)
	}

	headsigns, err := loadCSV(gtfsDir+"/trips.txt", "trip_id", "trip_headsign")
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

	err := readCSV(gtfsDir+"/trips.txt", []string{"route_id", "direction_id", "shape_id"}, func(row csvRow) {
		sid := row.get("shape_id")
		if sid == "" {
			return
		}
		k := dirKey{row.get("route_id"), row.get("direction_id")}
		if shapeCounts[k] == nil {
			shapeCounts[k] = make(map[string]int)
		}
		shapeCounts[k][sid]++
	})
	if err != nil {
		return nil, fmt.Errorf("trips: %w", err)
	}

	canonical := make(map[string]dirKey)
	for k, counts := range shapeCounts {
		best, bestN := "", 0
		for sid, n := range counts {
			if n > bestN || (n == bestN && sid < best) {
				best, bestN = sid, n
			}
		}
		canonical[best] = k
	}

	shapePoints := make(map[string][]shapePoint)
	err = readCSV(gtfsDir+"/shapes.txt", []string{"shape_id", "shape_pt_lat", "shape_pt_lon", "shape_pt_sequence"}, func(row csvRow) {
		sid := row.get("shape_id")
		if _, needed := canonical[sid]; !needed {
			return
		}
		lat, err1 := strconv.ParseFloat(row.get("shape_pt_lat"), 64)
		lon, err2 := strconv.ParseFloat(row.get("shape_pt_lon"), 64)
		seq, err3 := strconv.Atoi(row.get("shape_pt_sequence"))
		if err1 != nil || err2 != nil || err3 != nil {
			return
		}
		shapePoints[sid] = append(shapePoints[sid], shapePoint{lat, lon, seq})
	})
	if err != nil {
		return nil, fmt.Errorf("shapes: %w", err)
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

func csvHeader(data []byte) []byte {
	line, _, _ := bytes.Cut(data, []byte{'\n'})
	return bytes.TrimSpace(bytes.TrimPrefix(line, []byte("\ufeff")))
}

func mergeCSV(busData, lrtData []byte) ([]byte, error) {
	idx := bytes.IndexByte(lrtData, '\n')
	if idx < 0 {
		return busData, nil
	}
	if len(bytes.TrimSpace(busData)) == 0 {
		return lrtData, nil
	}
	if !bytes.Equal(csvHeader(busData), csvHeader(lrtData)) {
		return mergeCSVByName(busData, lrtData)
	}
	out := busData
	if out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, lrtData[idx+1:]...), nil
}

func readAllCSV(data []byte) ([][]string, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 && len(rows[0]) > 0 {
		rows[0][0] = strings.TrimPrefix(rows[0][0], "\ufeff")
	}
	return rows, nil
}

func mergeCSVByName(busData, lrtData []byte) ([]byte, error) {
	busRows, err := readAllCSV(busData)
	if err != nil {
		return nil, err
	}
	lrtRows, err := readAllCSV(lrtData)
	if err != nil {
		return nil, err
	}

	header := busRows[0]
	col := make(map[string]int, len(header))
	for i, h := range header {
		col[h] = i
	}
	lrtHeader := lrtRows[0]
	for _, h := range lrtHeader {
		if _, ok := col[h]; !ok {
			col[h] = len(header)
			header = append(header, h)
		}
	}

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Write(header)
	out := make([]string, len(header))
	for _, row := range busRows[1:] {
		clear(out)
		copy(out, row)
		w.Write(out)
	}
	for _, row := range lrtRows[1:] {
		clear(out)
		for i, v := range row {
			if i < len(lrtHeader) {
				out[col[lrtHeader[i]]] = v
			}
		}
		w.Write(out)
	}
	w.Flush()
	return buf.Bytes(), w.Error()
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
		busData, exists := merged[name]
		if !exists {
			merged[name] = lrtData
			continue
		}
		if name == "agency.txt" {
			continue
		}
		data, err := mergeCSV(busData, lrtData)
		if err != nil {
			return fmt.Errorf("merge %s: %w", name, err)
		}
		merged[name] = data
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
