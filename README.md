# grt live map

Real-time map of Grand River Transit buses and the Ion LRT in Waterloo, Ontario.
Vehicles are drawn on a Leaflet map with route polylines, next-stop predictions,
and service alerts.

## Data

Public GTFS and GTFS-Realtime feeds published by the Region of Waterloo
(`webapps.regionofwaterloo.ca/api/grt-routes/api`). No API key required.

- Static GTFS zips (bus + LRT) are downloaded on startup, merged, and cached
  under `data/GTFS/`; refreshed every 24 h while running.
- Realtime feeds (vehicle positions, trip updates, alerts) are polled every
  30 s.

## Architecture

Single Go binary, no external services:

- `gtfs.go` — downloads/parses the static feeds (stops, routes, trips, shapes)
  and builds a GeoJSON FeatureCollection of route polylines.
- `realtime.go` — fetches the GTFS-RT protobuf feeds, joins vehicle positions
  with trip updates to compute each vehicle's next stops, and serializes a
  JSON snapshot.
- `server.go` — embedded static file server, `/api/shapes` for the GeoJSON,
  and `/events`, a Server-Sent Events endpoint fed by a fan-out hub that
  broadcasts every snapshot to all connected browsers.
- `static/index.html` — Leaflet frontend; connects to `/events` and updates
  markers in place.

## Run

Requires Go 1.21+.

```
go run .
```

Serves on `http://localhost:8080` and opens it in your default browser
(Windows / macOS / Linux).

## Known limitations

- No tests; feed schema changes upstream would need code changes.
- Route colors are assigned from a fixed palette, not official GRT branding.
- If the static GTFS download fails on first run with no cache, the program
  exits — it needs the schedule data to label stops and trips.
