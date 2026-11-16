package globalping

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These fixtures are trimmed copies of real API responses. They matter
// because the shape differs per measurement type: ping returns `timings` as
// an array of per-packet entries while dns and http return it as an object,
// and an earlier version of this client failed to parse ping for exactly
// that reason.
const pingOK = `{"id":"m1","status":"finished","results":[
 {"probe":{"city":"Moscow","country":"RU","network":"MTS","asn":8359},
  "result":{"status":"finished",
   "timings":[{"ttl":55,"rtt":8.65},{"ttl":55,"rtt":10.8}],
   "stats":{"min":8.6,"max":10.76,"avg":9.337,"total":3,"loss":0,"rcv":3,"drop":0}}}]}`

const pingAllLost = `{"id":"m2","status":"finished","results":[
 {"probe":{"city":"Tomsk","country":"RU","network":"Someone","asn":1},
  "result":{"status":"finished","timings":[],
   "stats":{"total":3,"loss":100,"rcv":0,"drop":3}}}]}`

const dnsOK = `{"id":"m3","status":"finished","results":[
 {"probe":{"city":"Moscow","country":"RU","network":"Selectel","asn":49505},
  "result":{"status":"finished","statusCodeName":"NOERROR","statusCode":0,
   "answers":[{"name":"example.com.","type":"A","ttl":2,"class":"IN","value":"8.47.69.0"}],
   "timings":{"total":14},"resolver":"1.1.1.1"}}]}`

// A refusal still proves the packet made the round trip.
const dnsRefused = `{"id":"m4","status":"finished","results":[
 {"probe":{"city":"Moscow","country":"RU","network":"Selectel","asn":49505},
  "result":{"status":"finished","statusCodeName":"REFUSED","statusCode":5,
   "answers":[],"timings":{"total":21},"resolver":"203.0.113.1"}}]}`

const dnsTimedOut = `{"id":"m5","status":"finished","results":[
 {"probe":{"city":"Kemerovo","country":"RU","network":"GIS","asn":2},
  "result":{"status":"failed","failureSource":"resolver",
   "rawOutput":";; communications error to 203.0.113.1#34567: timed out\n;; no servers could be reached",
   "statusCodeName":null,"statusCode":null,"answers":[],"timings":{"total":0},"resolver":null}}]}`

const httpOK = `{"id":"m6","status":"finished","results":[
 {"probe":{"city":"Moscow","country":"RU","network":"Timeweb","asn":9123},
  "result":{"status":"finished","statusCode":200,"timings":{"total":120,"tls":40,"tcp":20}}}]}`

const httpRefused = `{"id":"m7","status":"finished","results":[
 {"probe":{"city":"Moscow","country":"RU","network":"Timeweb","asn":9123},
  "result":{"status":"failed","rawOutput":"connect ETIMEDOUT","statusCode":null,"timings":{"total":0}}}]}`

// serve returns a client pointed at a stub that answers every poll with the
// given body.
func serve(t *testing.T, body string) (*Client, *[]string) {
	t.Helper()
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			raw := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(raw)
			posted = append(posted, string(raw))
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"m1","probesCount":1}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.base = srv.URL
	return c, &posted
}

func TestPingVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		reached int
		detail  string
	}{
		{"answered", pingOK, 1, "9ms"},
		{"all packets lost", pingAllLost, 0, "100% потерь"},
	}
	for _, tc := range cases {
		c, _ := serve(t, tc.body)
		got, err := c.Ping(context.Background(), "203.0.113.1", "RU", 1)
		if err != nil {
			t.Fatalf("%s: Ping: %v", tc.name, err)
		}
		if got.Reached != tc.reached || got.Total != 1 {
			t.Errorf("%s: %s, want %d/1", tc.name, got.Summary(), tc.reached)
		}
		if got.Outcomes[0].Detail != tc.detail {
			t.Errorf("%s: detail = %q, want %q", tc.name, got.Outcomes[0].Detail, tc.detail)
		}
	}
}

func TestDNSVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		reached int
	}{
		{"answered", dnsOK, 1},
		// The distinction this whole tool rests on: a refusal is a reply.
		{"refused but replied", dnsRefused, 1},
		{"timed out", dnsTimedOut, 0},
	}
	for _, tc := range cases {
		c, _ := serve(t, tc.body)
		got, err := c.DNSOverUDP(context.Background(), "203.0.113.1", 53, "example.com", "RU", 1)
		if err != nil {
			t.Fatalf("%s: DNSOverUDP: %v", tc.name, err)
		}
		if got.Reached != tc.reached {
			t.Errorf("%s: %s, want %d/1", tc.name, got.Summary(), tc.reached)
		}
	}
}

func TestHTTPSVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		reached int
	}{
		{"served", httpOK, 1},
		{"timed out", httpRefused, 0},
	} {
		c, _ := serve(t, tc.body)
		got, err := c.HTTPS(context.Background(), "203.0.113.1", 443, "RU", 1)
		if err != nil {
			t.Fatalf("%s: HTTPS: %v", tc.name, err)
		}
		if got.Reached != tc.reached {
			t.Errorf("%s: %s, want %d/1", tc.name, got.Summary(), tc.reached)
		}
	}
}

func TestDNSRequestCarriesResolverAndPort(t *testing.T) {
	c, posted := serve(t, dnsOK)
	if _, err := c.DNSOverUDP(context.Background(), "203.0.113.9", 34567, "probe.test", "RU", 4); err != nil {
		t.Fatalf("DNSOverUDP: %v", err)
	}
	if len(*posted) == 0 {
		t.Fatal("nothing was posted")
	}

	var req struct {
		Type      string `json:"type"`
		Target    string `json:"target"`
		Limit     int    `json:"limit"`
		Locations []struct {
			Country string `json:"country"`
		} `json:"locations"`
		Options struct {
			Resolver string `json:"resolver"`
			Protocol string `json:"protocol"`
			Port     int    `json:"port"`
		} `json:"measurementOptions"`
	}
	if err := json.Unmarshal([]byte((*posted)[0]), &req); err != nil {
		t.Fatalf("request is not valid JSON: %v\n%s", err, (*posted)[0])
	}
	if req.Type != "dns" || req.Target != "probe.test" || req.Limit != 4 {
		t.Errorf("request = %+v", req)
	}
	if req.Options.Resolver != "203.0.113.9" || req.Options.Port != 34567 {
		t.Errorf("resolver/port = %s:%d", req.Options.Resolver, req.Options.Port)
	}
	if req.Options.Protocol != "UDP" {
		t.Errorf("protocol = %q, want UDP", req.Options.Protocol)
	}
	if len(req.Locations) != 1 || req.Locations[0].Country != "RU" {
		t.Errorf("locations = %+v", req.Locations)
	}
}

func TestFailuresNamesTheProbes(t *testing.T) {
	c, _ := serve(t, dnsTimedOut)
	got, err := c.DNSOverUDP(context.Background(), "203.0.113.1", 53, "example.com", "RU", 1)
	if err != nil {
		t.Fatalf("DNSOverUDP: %v", err)
	}
	failures := got.Failures()
	if len(failures) != 1 || !strings.Contains(failures[0], "Kemerovo") {
		t.Errorf("Failures() = %v", failures)
	}
}

func TestRateLimitIsReportedPlainly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"too many requests"}}`))
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.base = srv.URL

	_, err := c.Ping(context.Background(), "203.0.113.1", "RU", 1)
	if err == nil {
		t.Fatal("a 429 was treated as success")
	}
	if !strings.Contains(err.Error(), "allowance") {
		t.Errorf("error does not explain the limit: %v", err)
	}
}

func TestLimitsParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"rateLimit":{"measurements":{"create":{"type":"ip","limit":250,"remaining":94,"reset":2549}}}}`))
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.base = srv.URL

	remaining, reset, err := c.Limits(context.Background())
	if err != nil {
		t.Fatalf("Limits: %v", err)
	}
	if remaining != 94 || reset != 2549 {
		t.Errorf("remaining=%d reset=%d", remaining, reset)
	}
}

// A measurement from several groups of probes: each result carries its tags.
const pingGroups = `{"id":"m8","status":"finished","results":[
 {"probe":{"city":"Kostroma","country":"RU","network":"Rostelecom","asn":12389,"tags":["eyeball-network"]},
  "result":{"status":"finished","timings":[{"ttl":55,"rtt":40}],"stats":{"avg":40,"loss":0,"rcv":3}}},
 {"probe":{"city":"Moscow","country":"RU","network":"Selectel","asn":49505,"tags":["datacenter-network","u-x"]},
  "result":{"status":"finished","timings":[],"stats":{"loss":100,"rcv":0}}}]}`

func TestPingFromAsksEveryGroupWithItsOwnLimitAndReadsTheTags(t *testing.T) {
	c, posted := serve(t, pingGroups)
	res, err := c.PingFrom(context.Background(), "203.0.113.9", []Location{
		{Country: "RU", Tags: []string{TagEyeball}, Limit: 9},
		{Country: "DE", Limit: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal([]byte((*posted)[0]), &req); err != nil {
		t.Fatal(err)
	}
	if _, global := req["limit"]; global {
		t.Errorf("a global limit next to per-place limits is refused by the API: %s", (*posted)[0])
	}
	locs, _ := req["locations"].([]any)
	if len(locs) != 2 {
		t.Fatalf("locations: %s", (*posted)[0])
	}
	first := locs[0].(map[string]any)
	if first["country"] != "RU" || first["limit"] != float64(9) || first["tags"].([]any)[0] != TagEyeball {
		t.Errorf("first place: %v", first)
	}
	if res.Total != 2 || res.Reached != 1 {
		t.Fatalf("result: %+v", res)
	}
	if !res.Outcomes[0].Probe.HasTag(TagEyeball) || res.Outcomes[1].Probe.HasTag(TagEyeball) ||
		!res.Outcomes[1].Probe.HasTag(TagDatacenter) {
		t.Errorf("tags: %+v", res.Outcomes)
	}
}

const dnsOurs = `{"id":"m9","status":"finished","results":[
 {"probe":{"city":"Kostroma","country":"RU","network":"Rostelecom","asn":12389},
  "result":{"status":"finished","statusCodeName":"NOERROR","answers":[{"name":"x.","type":"A","value":"192.0.2.1"}],"timings":{"total":30}}},
 {"probe":{"city":"Tula","country":"RU","network":"ER-Telecom","asn":52207},
  "result":{"status":"finished","statusCodeName":"NOERROR","answers":[{"name":"x.","type":"A","value":"10.10.10.10"}],"timings":{"total":30}}},
 {"probe":{"city":"Ufa","country":"RU","network":"Vimpelcom","asn":3216},
  "result":{"status":"finished","statusCodeName":"REFUSED","answers":[],"timings":{"total":30}}},
 {"probe":{"city":"Kursk","country":"RU","network":"Kursktelecom","asn":42277},
  "result":{"status":"failed","rawOutput":"timed out","answers":[],"timings":{"total":0}}}]}`

func TestTheSameProbesAreAskedAndOnlyOurOwnAnswerCounts(t *testing.T) {
	c, posted := serve(t, dnsOurs)
	res, err := c.AskSameProbes(context.Background(), "m8", "203.0.113.9", 52886, "abc.example.com", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Type      string `json:"type"`
		Target    string `json:"target"`
		Locations string `json:"locations"`
		Options   struct {
			Resolver string `json:"resolver"`
			Protocol string `json:"protocol"`
			Port     int    `json:"port"`
		} `json:"measurementOptions"`
	}
	if err := json.Unmarshal([]byte((*posted)[0]), &req); err != nil {
		t.Fatalf("%v\n%s", err, (*posted)[0])
	}
	if req.Type != "dns" || req.Target != "abc.example.com" || req.Locations != "m8" ||
		req.Options.Resolver != "203.0.113.9" || req.Options.Port != 52886 || req.Options.Protocol != "UDP" {
		t.Errorf("request: %s", (*posted)[0])
	}
	// A different address or a refusal comes from something else that
	// answered, not from the server.
	if res.Total != 4 || res.Reached != 1 || !res.Outcomes[0].OK {
		t.Fatalf("result: %+v", res)
	}
	for _, o := range res.Outcomes[1:] {
		if o.OK || o.Detail == "" {
			t.Errorf("%+v should not count and should say why", o)
		}
	}
}
