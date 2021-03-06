// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package retrieval

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	clientmodel "github.com/prometheus/client_golang/model"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/util/httputil"
)

func TestBaseLabels(t *testing.T) {
	target := newTestTarget("example.com:80", 0, clientmodel.LabelSet{"job": "some_job", "foo": "bar"})
	want := clientmodel.LabelSet{
		clientmodel.JobLabel:      "some_job",
		clientmodel.InstanceLabel: "example.com:80",
		"foo": "bar",
	}
	got := target.BaseLabels()
	if !reflect.DeepEqual(want, got) {
		t.Errorf("want base labels %v, got %v", want, got)
	}
}

func TestOverwriteLabels(t *testing.T) {
	type test struct {
		metric       string
		resultNormal clientmodel.Metric
		resultHonor  clientmodel.Metric
	}
	var tests []test

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", `text/plain; version=0.0.4`)
				for _, test := range tests {
					w.Write([]byte(test.metric))
					w.Write([]byte(" 1\n"))
				}
			},
		),
	)
	defer server.Close()
	addr := clientmodel.LabelValue(strings.Split(server.URL, "://")[1])

	tests = []test{
		{
			metric: `foo{}`,
			resultNormal: clientmodel.Metric{
				clientmodel.MetricNameLabel: "foo",
				clientmodel.InstanceLabel:   addr,
			},
			resultHonor: clientmodel.Metric{
				clientmodel.MetricNameLabel: "foo",
				clientmodel.InstanceLabel:   addr,
			},
		},
		{
			metric: `foo{instance=""}`,
			resultNormal: clientmodel.Metric{
				clientmodel.MetricNameLabel: "foo",
				clientmodel.InstanceLabel:   addr,
			},
			resultHonor: clientmodel.Metric{
				clientmodel.MetricNameLabel: "foo",
			},
		},
		{
			metric: `foo{instance="other_instance"}`,
			resultNormal: clientmodel.Metric{
				clientmodel.MetricNameLabel:                                 "foo",
				clientmodel.InstanceLabel:                                   addr,
				clientmodel.ExportedLabelPrefix + clientmodel.InstanceLabel: "other_instance",
			},
			resultHonor: clientmodel.Metric{
				clientmodel.MetricNameLabel: "foo",
				clientmodel.InstanceLabel:   "other_instance",
			},
		},
	}

	target := newTestTarget(server.URL, 10*time.Millisecond, nil)

	target.honorLabels = false
	app := &collectResultAppender{}
	if err := target.scrape(app); err != nil {
		t.Fatal(err)
	}

	for i, test := range tests {
		if !reflect.DeepEqual(app.result[i].Metric, test.resultNormal) {
			t.Errorf("Error comparing %q:\nExpected:\n%s\nGot:\n%s\n", test.metric, test.resultNormal, app.result[i].Metric)
		}
	}

	target.honorLabels = true
	app = &collectResultAppender{}
	if err := target.scrape(app); err != nil {
		t.Fatal(err)
	}

	for i, test := range tests {
		if !reflect.DeepEqual(app.result[i].Metric, test.resultHonor) {
			t.Errorf("Error comparing %q:\nExpected:\n%s\nGot:\n%s\n", test.metric, test.resultHonor, app.result[i].Metric)
		}

	}
}
func TestTargetScrapeUpdatesState(t *testing.T) {
	testTarget := newTestTarget("bad schema", 0, nil)

	testTarget.scrape(nopAppender{})
	if testTarget.status.Health() != HealthBad {
		t.Errorf("Expected target state %v, actual: %v", HealthBad, testTarget.status.Health())
	}
}

func TestTargetScrapeWithFullChannel(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", `text/plain; version=0.0.4`)
				for i := 0; i < 2*ingestedSamplesCap; i++ {
					w.Write([]byte(
						fmt.Sprintf("test_metric_%d{foo=\"bar\"} 123.456\n", i),
					))
				}
			},
		),
	)
	defer server.Close()

	testTarget := newTestTarget(server.URL, 10*time.Millisecond, clientmodel.LabelSet{"dings": "bums"})

	testTarget.scrape(slowAppender{})
	if testTarget.status.Health() != HealthBad {
		t.Errorf("Expected target state %v, actual: %v", HealthBad, testTarget.status.Health())
	}
	if testTarget.status.LastError() != errIngestChannelFull {
		t.Errorf("Expected target error %q, actual: %q", errIngestChannelFull, testTarget.status.LastError())
	}
}

func TestTargetScrapeMetricRelabelConfigs(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", `text/plain; version=0.0.4`)
				w.Write([]byte("test_metric_drop 0\n"))
				w.Write([]byte("test_metric_relabel 1\n"))
			},
		),
	)
	defer server.Close()
	testTarget := newTestTarget(server.URL, 10*time.Millisecond, clientmodel.LabelSet{})
	testTarget.metricRelabelConfigs = []*config.RelabelConfig{
		{
			SourceLabels: clientmodel.LabelNames{"__name__"},
			Regex:        &config.Regexp{*regexp.MustCompile(".*drop.*")},
			Action:       config.RelabelDrop,
		},
		{
			SourceLabels: clientmodel.LabelNames{"__name__"},
			Regex:        &config.Regexp{*regexp.MustCompile(".*(relabel|up).*")},
			TargetLabel:  "foo",
			Replacement:  "bar",
			Action:       config.RelabelReplace,
		},
	}

	appender := &collectResultAppender{}
	testTarget.scrape(appender)

	// Remove variables part of result.
	for _, sample := range appender.result {
		sample.Timestamp = 0
		sample.Value = 0
	}

	expected := []*clientmodel.Sample{
		{
			Metric: clientmodel.Metric{
				clientmodel.MetricNameLabel: "test_metric_relabel",
				"foo": "bar",
				clientmodel.InstanceLabel: clientmodel.LabelValue(testTarget.url.Host),
			},
			Timestamp: 0,
			Value:     0,
		},
		// The metrics about the scrape are not affected.
		{
			Metric: clientmodel.Metric{
				clientmodel.MetricNameLabel: scrapeHealthMetricName,
				clientmodel.InstanceLabel:   clientmodel.LabelValue(testTarget.url.Host),
			},
			Timestamp: 0,
			Value:     0,
		},
		{
			Metric: clientmodel.Metric{
				clientmodel.MetricNameLabel: scrapeDurationMetricName,
				clientmodel.InstanceLabel:   clientmodel.LabelValue(testTarget.url.Host),
			},
			Timestamp: 0,
			Value:     0,
		},
	}

	if !appender.result.Equal(expected) {
		t.Fatalf("Expected and actual samples not equal. Expected: %s, actual: %s", expected, appender.result)
	}

}

func TestTargetRecordScrapeHealth(t *testing.T) {
	testTarget := newTestTarget("example.url:80", 0, clientmodel.LabelSet{clientmodel.JobLabel: "testjob"})

	now := clientmodel.Now()
	appender := &collectResultAppender{}
	testTarget.status.setLastError(nil)
	recordScrapeHealth(appender, now, testTarget.BaseLabels(), testTarget.status.Health(), 2*time.Second)

	result := appender.result

	if len(result) != 2 {
		t.Fatalf("Expected two samples, got %d", len(result))
	}

	actual := result[0]
	expected := &clientmodel.Sample{
		Metric: clientmodel.Metric{
			clientmodel.MetricNameLabel: scrapeHealthMetricName,
			clientmodel.InstanceLabel:   "example.url:80",
			clientmodel.JobLabel:        "testjob",
		},
		Timestamp: now,
		Value:     1,
	}

	if !actual.Equal(expected) {
		t.Fatalf("Expected and actual samples not equal. Expected: %v, actual: %v", expected, actual)
	}

	actual = result[1]
	expected = &clientmodel.Sample{
		Metric: clientmodel.Metric{
			clientmodel.MetricNameLabel: scrapeDurationMetricName,
			clientmodel.InstanceLabel:   "example.url:80",
			clientmodel.JobLabel:        "testjob",
		},
		Timestamp: now,
		Value:     2.0,
	}

	if !actual.Equal(expected) {
		t.Fatalf("Expected and actual samples not equal. Expected: %v, actual: %v", expected, actual)
	}
}

func TestTargetScrapeTimeout(t *testing.T) {
	signal := make(chan bool, 1)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				<-signal
				w.Header().Set("Content-Type", `text/plain; version=0.0.4`)
				w.Write([]byte{})
			},
		),
	)
	defer server.Close()

	testTarget := newTestTarget(server.URL, 50*time.Millisecond, clientmodel.LabelSet{})

	appender := nopAppender{}

	// scrape once without timeout
	signal <- true
	if err := testTarget.scrape(appender); err != nil {
		t.Fatal(err)
	}

	// let the deadline lapse
	time.Sleep(55 * time.Millisecond)

	// now scrape again
	signal <- true
	if err := testTarget.scrape(appender); err != nil {
		t.Fatal(err)
	}

	// now timeout
	if err := testTarget.scrape(appender); err == nil {
		t.Fatal("expected scrape to timeout")
	} else {
		signal <- true // let handler continue
	}

	// now scrape again without timeout
	signal <- true
	if err := testTarget.scrape(appender); err != nil {
		t.Fatal(err)
	}
}

func TestTargetScrape404(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
		),
	)
	defer server.Close()

	testTarget := newTestTarget(server.URL, 10*time.Millisecond, clientmodel.LabelSet{})
	appender := nopAppender{}

	want := errors.New("server returned HTTP status 404 Not Found")
	got := testTarget.scrape(appender)
	if got == nil || want.Error() != got.Error() {
		t.Fatalf("want err %q, got %q", want, got)
	}
}

func TestTargetRunScraperScrapes(t *testing.T) {
	testTarget := newTestTarget("bad schema", 0, nil)

	go testTarget.RunScraper(nopAppender{})

	// Enough time for a scrape to happen.
	time.Sleep(10 * time.Millisecond)
	if testTarget.status.LastScrape().IsZero() {
		t.Errorf("Scrape hasn't occured.")
	}

	testTarget.StopScraper()
	// Wait for it to take effect.
	time.Sleep(5 * time.Millisecond)
	last := testTarget.status.LastScrape()
	// Enough time for a scrape to happen.
	time.Sleep(10 * time.Millisecond)
	if testTarget.status.LastScrape() != last {
		t.Errorf("Scrape occured after it was stopped.")
	}
}

func BenchmarkScrape(b *testing.B) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", `text/plain; version=0.0.4`)
				w.Write([]byte("test_metric{foo=\"bar\"} 123.456\n"))
			},
		),
	)
	defer server.Close()

	testTarget := newTestTarget(server.URL, 100*time.Millisecond, clientmodel.LabelSet{"dings": "bums"})
	appender := nopAppender{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := testTarget.scrape(appender); err != nil {
			b.Fatal(err)
		}
	}
}

func TestURLParams(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", `text/plain; version=0.0.4`)
				w.Write([]byte{})
				r.ParseForm()
				if r.Form["foo"][0] != "bar" {
					t.Fatalf("URL parameter 'foo' had unexpected first value '%v'", r.Form["foo"][0])
				}
				if r.Form["foo"][1] != "baz" {
					t.Fatalf("URL parameter 'foo' had unexpected second value '%v'", r.Form["foo"][1])
				}
			},
		),
	)
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	target := NewTarget(
		&config.ScrapeConfig{
			JobName:        "test_job1",
			ScrapeInterval: config.Duration(1 * time.Minute),
			ScrapeTimeout:  config.Duration(1 * time.Second),
			Scheme:         serverURL.Scheme,
			Params: url.Values{
				"foo": []string{"bar", "baz"},
			},
		},
		clientmodel.LabelSet{
			clientmodel.SchemeLabel:  clientmodel.LabelValue(serverURL.Scheme),
			clientmodel.AddressLabel: clientmodel.LabelValue(serverURL.Host),
			"__param_foo":            "bar",
		},
		nil)
	app := &collectResultAppender{}
	if err = target.scrape(app); err != nil {
		t.Fatal(err)
	}
}

func newTestTarget(targetURL string, deadline time.Duration, baseLabels clientmodel.LabelSet) *Target {
	t := &Target{
		url: &url.URL{
			Scheme: "http",
			Host:   strings.TrimLeft(targetURL, "http://"),
			Path:   "/metrics",
		},
		deadline:        deadline,
		status:          &TargetStatus{},
		scrapeInterval:  1 * time.Millisecond,
		httpClient:      httputil.NewDeadlineClient(deadline, nil),
		scraperStopping: make(chan struct{}),
		scraperStopped:  make(chan struct{}),
	}
	t.baseLabels = clientmodel.LabelSet{
		clientmodel.InstanceLabel: clientmodel.LabelValue(t.InstanceIdentifier()),
	}
	for baseLabel, baseValue := range baseLabels {
		t.baseLabels[baseLabel] = baseValue
	}
	return t
}

func TestNewHTTPBearerToken(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				expected := "Bearer 1234"
				received := r.Header.Get("Authorization")
				if expected != received {
					t.Fatalf("Authorization header was not set correctly: expected '%v', got '%v'", expected, received)
				}
			},
		),
	)
	defer server.Close()

	cfg := &config.ScrapeConfig{
		ScrapeTimeout: config.Duration(1 * time.Second),
		BearerToken:   "1234",
	}
	c, err := newHTTPClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewHTTPBearerTokenFile(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				expected := "Bearer 12345"
				received := r.Header.Get("Authorization")
				if expected != received {
					t.Fatalf("Authorization header was not set correctly: expected '%v', got '%v'", expected, received)
				}
			},
		),
	)
	defer server.Close()

	cfg := &config.ScrapeConfig{
		ScrapeTimeout:   config.Duration(1 * time.Second),
		BearerTokenFile: "testdata/bearertoken.txt",
	}
	c, err := newHTTPClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewHTTPCACert(t *testing.T) {
	server := httptest.NewUnstartedServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", `text/plain; version=0.0.4`)
				w.Write([]byte{})
			},
		),
	)
	server.TLS = newTLSConfig(t)
	server.StartTLS()
	defer server.Close()

	cfg := &config.ScrapeConfig{
		ScrapeTimeout: config.Duration(1 * time.Second),
		CACert:        "testdata/ca.cer",
	}
	c, err := newHTTPClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewHTTPClientCert(t *testing.T) {
	server := httptest.NewUnstartedServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", `text/plain; version=0.0.4`)
				w.Write([]byte{})
			},
		),
	)
	tlsConfig := newTLSConfig(t)
	tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	tlsConfig.ClientCAs = tlsConfig.RootCAs
	tlsConfig.BuildNameToCertificate()
	server.TLS = tlsConfig
	server.StartTLS()
	defer server.Close()

	cfg := &config.ScrapeConfig{
		ScrapeTimeout: config.Duration(1 * time.Second),
		CACert:        "testdata/ca.cer",
		ClientCert: &config.ClientCert{
			Cert: "testdata/client.cer",
			Key:  "testdata/client.key",
		},
	}
	c, err := newHTTPClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
}

func newTLSConfig(t *testing.T) *tls.Config {
	tlsConfig := &tls.Config{}
	caCertPool := x509.NewCertPool()
	caCert, err := ioutil.ReadFile("testdata/ca.cer")
	if err != nil {
		t.Fatalf("Couldn't set up TLS server: %v", err)
	}
	caCertPool.AppendCertsFromPEM(caCert)
	tlsConfig.RootCAs = caCertPool
	tlsConfig.ServerName = "127.0.0.1"
	cert, err := tls.LoadX509KeyPair("testdata/server.cer", "testdata/server.key")
	if err != nil {
		t.Errorf("Unable to use specified server cert (%s) & key (%v): %s", "testdata/server.cer", "testdata/server.key")
	}
	tlsConfig.Certificates = []tls.Certificate{cert}
	tlsConfig.BuildNameToCertificate()
	return tlsConfig
}
