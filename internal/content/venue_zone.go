package content

import (
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/bradfitz/latlong"
)

// Coordinate patterns found in the map links people paste. All are linear-time
// (RE2) and only ever run on a URL already validated by validateMapURL (https,
// known map host, at most 2048 runes).
var (
	// Google place pin: .../data=!3d25.1972!4d55.2744 (latitude, then longitude). The most precise form.
	reMapPin = regexp.MustCompile(`!3d(-?\d{1,2}(?:\.\d+)?)!4d(-?\d{1,3}(?:\.\d+)?)`)
	// Google embed link: ...!2d55.2744!3d25.1972 (longitude, then latitude).
	reMapEmbed = regexp.MustCompile(`!2d(-?\d{1,3}(?:\.\d+)?)!3d(-?\d{1,2}(?:\.\d+)?)`)
	// Google viewport centre: .../@25.1972,55.2744,17z
	reMapAt = regexp.MustCompile(`@(-?\d{1,2}(?:\.\d+)?),(-?\d{1,3}(?:\.\d+)?)`)
	// OpenStreetMap: #map=17/25.1972/55.2744
	reMapOSM = regexp.MustCompile(`map=\d{1,2}/(-?\d{1,2}(?:\.\d+)?)/(-?\d{1,3}(?:\.\d+)?)`)
	// A bare "lat,lng" query value.
	reLatLng = regexp.MustCompile(`^\s*(-?\d{1,2}(?:\.\d+)?)\s*,\s*(-?\d{1,3}(?:\.\d+)?)\s*$`)
)

// mapCoordinateParams are the query parameters that carry "lat,lng" on the
// supported map hosts (Google, Apple).
var mapCoordinateParams = []string{"ll", "q", "query", "center", "destination", "coordinate", "sll"}

// mapCoordinates extracts a latitude/longitude from a validated map URL. It
// never follows redirects or makes a network call, so shortened links
// (maps.app.goo.gl, goo.gl/maps) and address-only links yield ok == false.
// (0, 0) is rejected: it is the usual placeholder, not a real venue.
func mapCoordinates(canon string) (lat, lon float64, ok bool) {
	try := func(latS, lonS string) bool {
		la, err1 := strconv.ParseFloat(latS, 64)
		lo, err2 := strconv.ParseFloat(lonS, 64)
		if err1 != nil || err2 != nil || math.Abs(la) > 90 || math.Abs(lo) > 180 || (la == 0 && lo == 0) {
			return false
		}
		lat, lon = la, lo
		return true
	}
	if m := reMapPin.FindStringSubmatch(canon); m != nil && try(m[1], m[2]) {
		return lat, lon, true
	}
	if m := reMapEmbed.FindStringSubmatch(canon); m != nil && try(m[2], m[1]) {
		return lat, lon, true
	}
	if u, err := url.Parse(canon); err == nil {
		q := u.Query()
		for _, name := range mapCoordinateParams {
			if m := reLatLng.FindStringSubmatch(q.Get(name)); m != nil && try(m[1], m[2]) {
				return lat, lon, true
			}
		}
		if try(q.Get("mlat"), q.Get("mlon")) {
			return lat, lon, true
		}
	}
	if m := reMapOSM.FindStringSubmatch(canon); m != nil && try(m[1], m[2]) {
		return lat, lon, true
	}
	if m := reMapAt.FindStringSubmatch(canon); m != nil && try(m[1], m[2]) {
		return lat, lon, true
	}
	return 0, 0, false
}

// venueTimezone returns the IANA timezone of the first location block whose
// map link carries coordinates, or "" when none does (no map link, a shortened
// or address-only link, an invalid link, or coordinates over open water). It
// tolerates malformed blocks: the main validation pass reports those.
func venueTimezone(in []json.RawMessage) string {
	for _, raw := range in {
		var meta blockMeta
		if err := json.Unmarshal(raw, &meta); err != nil || meta.Type != "location" {
			continue
		}
		var b locationBlockJSON
		if err := strictUnmarshal(raw, &b); err != nil || b.MapURL == "" {
			continue
		}
		canon, ok := validateMapURL(b.MapURL, 2048)
		if !ok {
			continue
		}
		lat, lon, ok := mapCoordinates(canon)
		if !ok {
			continue
		}
		name := latlong.LookupZoneName(lat, lon)
		if name == "" || name == "Local" {
			continue
		}
		if _, err := time.LoadLocation(name); err != nil {
			continue
		}
		return name
	}
	return ""
}
