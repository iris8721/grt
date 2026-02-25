package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	gtfs "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

func fetchFeed(url string) (*gtfs.FeedMessage, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	feed := &gtfs.FeedMessage{}
	if err := proto.Unmarshal(body, feed); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return feed, nil
}

type Snapshot struct {
	BusVP     *gtfs.FeedMessage
	BusTU     *gtfs.FeedMessage
	LrtVP     *gtfs.FeedMessage
	LrtTU     *gtfs.FeedMessage
	Alerts    *gtfs.FeedMessage
	Errors    []string
	FetchedAt time.Time
}

func fetchAll() *Snapshot {
	type result struct {
		key  string
		feed *gtfs.FeedMessage
		err  error
	}

	feeds := []struct{ key, url string }{
		{"busVP", busVehiclePositionsURL},
		{"busTU", busTripUpdatesURL},
		{"lrtVP", lrtVehiclePositionsURL},
		{"lrtTU", lrtTripUpdatesURL},
		{"alerts", alertsURL},
	}

	ch := make(chan result, len(feeds))
	for _, f := range feeds {
		f := f
		go func() {
			feed, err := fetchFeed(f.url)
			ch <- result{f.key, feed, err}
		}()
	}

	snap := &Snapshot{FetchedAt: time.Now()}
	for range feeds {
		r := <-ch
		if r.err != nil {
			snap.Errors = append(snap.Errors, fmt.Sprintf("%s: %v", r.key, r.err))
			continue
		}
		switch r.key {
		case "busVP":
			snap.BusVP = r.feed
		case "busTU":
			snap.BusTU = r.feed
		case "lrtVP":
			snap.LrtVP = r.feed
		case "lrtTU":
			snap.LrtTU = r.feed
		case "alerts":
			snap.Alerts = r.feed
		}
	}
	return snap
}

type StopMsg struct {
	Name    string `json:"name"`
	ArrTime string `json:"arr"`
	DelayS  int32  `json:"delay"`
}

type VehicleMsg struct {
	ID        string    `json:"id"`
	RouteID   string    `json:"route_id"`
	RouteName string    `json:"route_name"`
	Headsign  string    `json:"headsign"`
	Lat       float64   `json:"lat"`
	Lon       float64   `json:"lon"`
	DelayS    int32     `json:"delay_s"`
	Type      string    `json:"type"`
	Color     string    `json:"color"`
	NextStops []StopMsg `json:"next_stops"`
}

type AlertMsg struct {
	ID     string   `json:"id"`
	Routes []string `json:"routes"`
	Desc   string   `json:"desc"`
}

type SSEMessage struct {
	Ts          string       `json:"ts"`
	GTFSNextMin int          `json:"gtfs_next_min"`
	Vehicles    []VehicleMsg `json:"vehicles"`
	Alerts      []AlertMsg   `json:"alerts"`
	Errors      []string     `json:"errors,omitempty"`
}

var htmlTag = regexp.MustCompile(`<[^>]+>`)

func stripHTML(s string) string {
	s = htmlTag.ReplaceAllString(s, " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

func buildTUMap(feed *gtfs.FeedMessage) map[string][]*gtfs.TripUpdate_StopTimeUpdate {
	m := make(map[string][]*gtfs.TripUpdate_StopTimeUpdate)
	if feed == nil {
		return m
	}
	for _, entity := range feed.Entity {
		tu := entity.TripUpdate
		if tu == nil || len(tu.StopTimeUpdate) == 0 {
			continue
		}
		m[tu.Trip.GetTripId()] = tu.StopTimeUpdate
	}
	return m
}

const maxNextStops = 5

func buildVehicles(vpFeed *gtfs.FeedMessage, tuFeed *gtfs.FeedMessage, vehicleType string, sd *StaticData) []VehicleMsg {
	if vpFeed == nil {
		return nil
	}
	tuMap := buildTUMap(tuFeed)

	var out []VehicleMsg
	for _, entity := range vpFeed.Entity {
		v := entity.Vehicle
		if v == nil {
			continue
		}
		pos := v.Position
		trip := v.Trip
		routeID := trip.GetRouteId()
		tripID := trip.GetTripId()

		stus := tuMap[tripID]
		delay := int32(0)
		var nextStops []StopMsg
		for i, stu := range stus {
			if i >= maxNextStops {
				break
			}
			d := int32(0)
			arrTime := ""
			if stu.Arrival != nil {
				d = stu.Arrival.GetDelay()
				if t := stu.Arrival.GetTime(); t != 0 {
					arrTime = time.Unix(t, 0).Local().Format("15:04")
				}
			}
			if i == 0 {
				delay = d
			}
			nextStops = append(nextStops, StopMsg{
				Name:    sd.stopName(stu.GetStopId()),
				ArrTime: arrTime,
				DelayS:  d,
			})
		}

		out = append(out, VehicleMsg{
			ID:        v.Vehicle.GetId(),
			RouteID:   routeID,
			RouteName: sd.routeName(routeID),
			Headsign:  sd.headsign(tripID),
			Lat:       float64(pos.GetLatitude()),
			Lon:       float64(pos.GetLongitude()),
			DelayS:    delay,
			Type:      vehicleType,
			Color:     sd.color(routeID),
			NextStops: nextStops,
		})
	}
	return out
}

func buildMessage(snap *Snapshot, sd *StaticData, gtfsUpdatedAt time.Time) []byte {
	vehicles := buildVehicles(snap.BusVP, snap.BusTU, "bus", sd)
	vehicles = append(vehicles, buildVehicles(snap.LrtVP, snap.LrtTU, "lrt", sd)...)

	var alerts []AlertMsg
	if snap.Alerts != nil {
		for _, entity := range snap.Alerts.Entity {
			a := entity.Alert
			if a == nil {
				continue
			}
			var routes []string
			for _, ie := range a.InformedEntity {
				if rid := ie.GetRouteId(); rid != "" {
					routes = append(routes, sd.routeName(rid))
				}
			}
			desc := ""
			for _, t := range a.DescriptionText.GetTranslation() {
				if t.GetLanguage() == "en" || desc == "" {
					desc = stripHTML(t.GetText())
				}
			}
			const maxDesc = 200
			if len(desc) > maxDesc {
				desc = desc[:maxDesc] + "…"
			}
			alerts = append(alerts, AlertMsg{
				ID:     entity.GetId(),
				Routes: routes,
				Desc:   desc,
			})
		}
	}

	nextMin := int(time.Until(gtfsUpdatedAt.Add(gtfsUpdateInterval)).Minutes())
	if nextMin < 0 {
		nextMin = 0
	}

	b, _ := json.Marshal(SSEMessage{
		Ts:          snap.FetchedAt.Format("15:04:05"),
		GTFSNextMin: nextMin,
		Vehicles:    vehicles,
		Alerts:      alerts,
		Errors:      snap.Errors,
	})
	return b
}
