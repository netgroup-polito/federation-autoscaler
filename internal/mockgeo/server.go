/*
Copyright 2026 Politecnico di Torino - NetGroup.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package mockgeo is a tiny stand-in for an ip-api.com-style geolocation service
// used by the automatic-location-discovery feature. It is IP-KEYED: GET /json/{ip}
// returns the location of that IP, resolved by LONGEST-PREFIX CIDR match against a
// compiled-in table (a real geo-IP service works the same way — by prefix, not by
// hashing). Both agent roles read their own node IP and query this service; the
// discovered lat/lon drive the latency strategy and the returned region code keys
// the carbon (mock-eco) lookup for the eco strategy. The Broker never calls it.
//
// The table is filled with the operator's private subnets carved fine enough to
// separate co-located clusters: the demo's four /24s (172.23.{4,6,7,22}.0/24) are
// split into /26 blocks, each mapped to a distinct real city, so two clusters
// sharing a /24 still land in different cities. Region codes match the carbon
// service (mock-eco), so one discovered location drives both strategies.
package mockgeo

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
)

// GeoResponse is the ip-api.com-style JSON shape returned by GET /json/{ip} and
// consumed by the agent's geo client. Query/Status/Message form the response
// envelope; the rest is the location itself.
type GeoResponse struct {
	Query         string  `json:"query"`
	Status        string  `json:"status"`
	Message       string  `json:"message,omitempty"`
	ContinentCode string  `json:"continentCode,omitempty"`
	CountryCode   string  `json:"countryCode,omitempty"`
	Region        string  `json:"region,omitempty"`
	RegionName    string  `json:"regionName,omitempty"`
	City          string  `json:"city,omitempty"`
	Lat           float64 `json:"lat,omitempty"`
	Lon           float64 `json:"lon,omitempty"`
	ISP           string  `json:"isp,omitempty"`
	Org           string  `json:"org,omitempty"`
	AS            string  `json:"as,omitempty"`
}

// geoRow is one location fronting one or more CIDRs. A city can front several
// subnets (e.g. the .0/26 and .192/26 blocks of a /24 share a city).
type geoRow struct {
	cidrs []string
	loc   GeoResponse
}

// geoTable maps the operator's private subnets to real cities. Each /24 hosts
// three distinct cities on its .0/.64/.128 /26 blocks; the .192/26 block reuses
// the .0 city so every host IP resolves to a real location. The 0.0.0.0/0 default
// row returns "no location" (empty region, zero coords) for any unmatched IP — the
// agent then advertises no coordinates and the eco strategy falls back. Region
// codes MUST exist in mock-eco (they are the carbon join key).
var geoTable = []geoRow{
	// 172.23.4.0/24 — Americas + Pacific.
	{[]string{"172.23.4.0/26", "172.23.4.192/26"}, GeoResponse{ContinentCode: "NA", CountryCode: "CA", Region: "QC", RegionName: "Quebec", City: "Montreal", Lat: 45.6085, Lon: -73.5493, ISP: "Le Groupe Videotron Ltee", Org: "Videotron Ltee", AS: "AS5769 Videotron Ltee"}},
	{[]string{"172.23.4.64/26"}, GeoResponse{ContinentCode: "NA", CountryCode: "US", Region: "CA", RegionName: "California", City: "San Jose", Lat: 37.3382, Lon: -121.8863, ISP: "Google LLC", Org: "Google Cloud", AS: "AS15169 Google LLC"}},
	{[]string{"172.23.4.128/26"}, GeoResponse{ContinentCode: "OC", CountryCode: "AU", Region: "NSW", RegionName: "New South Wales", City: "Sydney", Lat: -33.8688, Lon: 151.2093, ISP: "Telstra Corporation", Org: "Telstra", AS: "AS1221 Telstra Corporation Ltd"}},

	// 172.23.6.0/24 — Western Europe + East Asia.
	{[]string{"172.23.6.0/26", "172.23.6.192/26"}, GeoResponse{ContinentCode: "EU", CountryCode: "FR", Region: "IDF", RegionName: "Ile-de-France", City: "Paris", Lat: 48.8566, Lon: 2.3522, ISP: "Orange", Org: "Orange", AS: "AS3215 Orange S.A."}},
	{[]string{"172.23.6.64/26"}, GeoResponse{ContinentCode: "EU", CountryCode: "GB", Region: "ENG", RegionName: "England", City: "London", Lat: 51.5074, Lon: -0.1278, ISP: "British Telecommunications", Org: "BT", AS: "AS2856 British Telecommunications PLC"}},
	{[]string{"172.23.6.128/26"}, GeoResponse{ContinentCode: "AS", CountryCode: "JP", Region: "13", RegionName: "Tokyo", City: "Tokyo", Lat: 35.6895, Lon: 139.6917, ISP: "NTT Communications", Org: "NTT", AS: "AS2914 NTT America, Inc."}},

	// 172.23.7.0/24 — Central Europe + South America + South Asia.
	{[]string{"172.23.7.0/26", "172.23.7.192/26"}, GeoResponse{ContinentCode: "EU", CountryCode: "DE", Region: "HE", RegionName: "Hesse", City: "Frankfurt", Lat: 50.1109, Lon: 8.6821, ISP: "Amazon.com", Org: "AWS", AS: "AS16509 Amazon.com Services LLC"}},
	{[]string{"172.23.7.64/26"}, GeoResponse{ContinentCode: "SA", CountryCode: "BR", Region: "SP", RegionName: "Sao Paulo", City: "Sao Paulo", Lat: -23.5505, Lon: -46.6333, ISP: "Claro", Org: "Claro", AS: "AS28573 Claro S.A."}},
	{[]string{"172.23.7.128/26"}, GeoResponse{ContinentCode: "AS", CountryCode: "IN", Region: "MH", RegionName: "Maharashtra", City: "Mumbai", Lat: 19.0760, Lon: 72.8777, ISP: "Reliance Jio Infocomm", Org: "Jio", AS: "AS55836 Reliance Jio Infocomm Limited"}},

	// 172.23.22.0/24 — Nordics + Mediterranean + Southeast Asia.
	{[]string{"172.23.22.0/26", "172.23.22.192/26"}, GeoResponse{ContinentCode: "EU", CountryCode: "SE", Region: "AB", RegionName: "Stockholm", City: "Stockholm", Lat: 59.3293, Lon: 18.0686, ISP: "Telia Company", Org: "Telia", AS: "AS3301 Telia Company AB"}},
	{[]string{"172.23.22.64/26"}, GeoResponse{ContinentCode: "EU", CountryCode: "IT", Region: "LOM", RegionName: "Lombardy", City: "Milan", Lat: 45.4642, Lon: 9.1900, ISP: "Telecom Italia Mobile", Org: "Telecom Italia", AS: "AS3269 Telecom Italia S.p.a."}},
	{[]string{"172.23.22.128/26"}, GeoResponse{ContinentCode: "AS", CountryCode: "SG", Region: "SG", RegionName: "Singapore", City: "Singapore", Lat: 1.3521, Lon: 103.8198, ISP: "Singtel", Org: "Singtel", AS: "AS7473 Singapore Telecommunications Ltd"}},

	// Default: any IP outside the known private subnets → no location.
	{[]string{"0.0.0.0/0"}, GeoResponse{}},
}

// compiledRow is a geoRow with its CIDRs parsed to *net.IPNet for matching.
type compiledRow struct {
	nets []*net.IPNet
	loc  GeoResponse
}

// compiledTable is geoTable with CIDRs pre-parsed. Built once at package init; a
// malformed hardcoded CIDR is a programmer error and panics immediately.
var compiledTable = compile(geoTable)

func compile(rows []geoRow) []compiledRow {
	out := make([]compiledRow, 0, len(rows))
	for _, r := range rows {
		cr := compiledRow{loc: r.loc}
		for _, c := range r.cidrs {
			_, n, err := net.ParseCIDR(c)
			if err != nil {
				panic(fmt.Sprintf("mock-geo: bad CIDR %q: %v", c, err))
			}
			cr.nets = append(cr.nets, n)
		}
		out = append(out, cr)
	}
	return out
}

// lookup returns the location for ip by longest-prefix match. The 0.0.0.0/0 row
// (prefix length 0) is the fallback, so a match is always found; a more specific
// /26 row (prefix length 26) always beats it.
func lookup(ip net.IP) GeoResponse {
	var best GeoResponse
	bestOnes := -1
	for _, r := range compiledTable {
		for _, n := range r.nets {
			if !n.Contains(ip) {
				continue
			}
			if ones, _ := n.Mask.Size(); ones > bestOnes {
				bestOnes = ones
				best = r.loc
			}
		}
	}
	return best
}

// Handler serves GET /json/{ip}. An unparseable IP returns the ip-api "fail"
// envelope (HTTP 200, as ip-api does); a valid IP returns its matched location
// stamped with the query and status "success".
func Handler(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	w.Header().Set("Content-Type", "application/json")

	parsed := net.ParseIP(ip)
	if parsed == nil {
		if err := json.NewEncoder(w).Encode(GeoResponse{Query: ip, Status: "fail", Message: "invalid query"}); err != nil {
			log.Printf("mock-geo: encode error: %v", err)
		}
		return
	}

	loc := lookup(parsed)
	loc.Query = ip
	loc.Status = "success"
	if err := json.NewEncoder(w).Encode(loc); err != nil {
		log.Printf("mock-geo: encode error: %v", err)
	}
}

// StartServer starts the mock-geo HTTP server on the given port and blocks.
func StartServer(port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json/{ip}", Handler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	addr := fmt.Sprintf(":%d", port)
	log.Printf("mock-geo listening on %s", addr)
	return http.ListenAndServe(addr, mux)
}
