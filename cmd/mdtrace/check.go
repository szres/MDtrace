package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"mdtrace/internal/event"
	"mdtrace/internal/missions"
)

// geoSummary describes an event's map inputs; a zero value means the event has none.
type geoSummary struct {
	routes, coastlineWays int
}

// checkGeo validates geo/routes.json and geo/coastline.json against the mission list. Both files
// are optional together: without them the charts simply leave out the maps.
func checkGeo(ev *event.Event, list []missions.Mission) (geoSummary, error) {
	routesRaw, routesErr := os.ReadFile(ev.Path("geo", "routes.json"))
	coastRaw, coastErr := os.ReadFile(ev.Path("geo", "coastline.json"))
	if errors.Is(routesErr, os.ErrNotExist) && errors.Is(coastErr, os.ErrNotExist) {
		return geoSummary{}, nil
	}
	if routesErr != nil {
		return geoSummary{}, fmt.Errorf("geo: %w (routes.json and coastline.json go together)", routesErr)
	}
	if coastErr != nil {
		return geoSummary{}, fmt.Errorf("geo: %w (routes.json and coastline.json go together)", coastErr)
	}
	var routes struct {
		Routes []struct {
			MissionGUID string      `json:"missionGuid"`
			Points      [][]float64 `json:"points"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(routesRaw, &routes); err != nil {
		return geoSummary{}, fmt.Errorf("geo/routes.json: %w", err)
	}
	byGUID := map[string]int{}
	for _, r := range routes.Routes {
		if len(r.Points) < 2 {
			return geoSummary{}, fmt.Errorf("geo/routes.json: route %s has fewer than 2 points", r.MissionGUID)
		}
		for _, p := range r.Points {
			if len(p) != 2 {
				return geoSummary{}, fmt.Errorf("geo/routes.json: route %s has a point that is not [lat, lng]", r.MissionGUID)
			}
		}
		byGUID[r.MissionGUID]++
	}
	for _, m := range list {
		if byGUID[m.GUID] != 1 {
			return geoSummary{}, fmt.Errorf("geo/routes.json: mission #%02d %s needs exactly one route, found %d", m.Order, m.DisplayName, byGUID[m.GUID])
		}
	}
	if len(routes.Routes) != len(list) {
		return geoSummary{}, fmt.Errorf("geo/routes.json has %d routes for %d missions", len(routes.Routes), len(list))
	}
	var coast struct {
		Ways [][][]float64 `json:"ways"`
	}
	if err := json.Unmarshal(coastRaw, &coast); err != nil {
		return geoSummary{}, fmt.Errorf("geo/coastline.json: %w", err)
	}
	return geoSummary{routes: len(routes.Routes), coastlineWays: len(coast.Ways)}, nil
}

// checkEvent validates everything an event needs and writes a short summary.
func checkEvent(w io.Writer, ev *event.Event, list []missions.Mission) error {
	orders := map[int]bool{}
	for _, m := range list {
		orders[m.Order] = true
	}
	if f := ev.MapFocus; f != nil {
		for _, order := range f.Orders {
			if !orders[order] {
				return fmt.Errorf("mapFocus.orders: no mission #%02d in missions.json", order)
			}
		}
	}
	geo, err := checkGeo(ev, list)
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "event      %s · %s / %s\n", ev.Slug, ev.Name.EN, ev.Name.ZH)
	fmt.Fprintf(w, "time zone  %s\n", ev.Timezone)
	fmt.Fprintf(w, "missions   %d\n", len(list))
	if wx := ev.Weather; wx != nil {
		fmt.Fprintf(w, "weather    %.4f, %.4f (%s)\n", wx.Lat, wx.Lng, wx.Area.EN)
	} else {
		fmt.Fprintln(w, "weather    off (no weather point)")
	}
	if geo.routes == 0 {
		fmt.Fprintln(w, "map        off (no geo/)")
	} else {
		focus := "no zoom panel"
		if f := ev.MapFocus; f != nil {
			labels := make([]string, len(f.Orders))
			for i, order := range f.Orders {
				labels[i] = fmt.Sprintf("#%02d", order)
			}
			focus = fmt.Sprintf("zoom on %s (%s)", f.Label.EN, strings.Join(labels, " "))
		}
		fmt.Fprintf(w, "map        %d routes, %d coastline ways, %s\n", geo.routes, geo.coastlineWays, focus)
	}
	return nil
}

func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	flags := addEventFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ev, list, err := loadEventMissions(flags)
	if err != nil {
		return err
	}
	return checkEvent(os.Stdout, ev, list)
}
