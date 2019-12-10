// Copyright 2016 The Prometheus Authors
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

package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

type Sansay struct {
	XMLName  xml.Name `xml:"mysqldump"`
	Text     string   `xml:",chardata"`
	Database struct {
		Text  string `xml:",chardata"`
		Name  string `xml:"name,attr"`
		Table []struct {
			Text string `xml:",chardata"`
			Name string `xml:"name,attr"`
			Row  []struct {
				Text  string `xml:",chardata"`
				Field []struct {
					Text string `xml:",chardata"`
					Name string `xml:"name,attr"`
				} `xml:"field"`
			} `xml:"row"`
		} `xml:"table"`
	} `xml:"database"`
}

type Trunk struct {
	TrunkId    string
	Alias      string
	Fqdn       string
	NumOrig    string
	NumTerm    string
	Cps        string
	NumPeak    string
	TotalCLZ   string
	NumCLZCps  string
	TotalLimit string
	CpsLimit   string
}
type collector struct {
	target   string
	username string
	password string
	logger   log.Logger
}

// Describe implements Prometheus.Collector.
func (c collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("dummy", "dummy", nil, nil)
}

// Collect implements Prometheus.Collector.
func (c collector) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()
	sansay, err := ScrapeTarget(c.target, c.username, c.password, c.logger)
	if err != nil {
		level.Info(c.logger).Log("msg", "Error scraping target", "err", err)
		ch <- prometheus.NewInvalidMetric(prometheus.NewDesc("sansay_error", "Error scraping target", nil, nil), err)
		return
	}
	for _, table := range sansay.Database.Table {
		switch table.Name {
		case "system_stat":
			for _, row := range table.Row {
				for _, field := range row.Field {
					switch field.Name {
					case "ha_pre_state":
					case "ha_current_state":
					default:
						addMetric(ch, field.Name, field.Text)
					}
				}
			}
		case "XBResourceRealTimeStatList":
			for _, row := range table.Row {
				trunk := Trunk{}
				for _, field := range row.Field {
					err := setField(&trunk, field.Name, field.Text)
					if err != nil {
						ch <- prometheus.NewInvalidMetric(prometheus.NewDesc("sansay_error", "Error scraping target", nil, nil), err)
					}
				}
				if trunk.Fqdn == "Group" {
					err := addTrunkMetrics(ch, trunk)
					if err != nil {
						ch <- prometheus.NewInvalidMetric(prometheus.NewDesc("sansay_error", "Error scraping target", nil, nil), err)
					}
				}
			}
		}
	}

	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("sansay_scrape_duration_seconds", "Total sansay time scrape took (walk and processing).", nil, nil),
		prometheus.GaugeValue,
		time.Since(start).Seconds())
}

func ScrapeTarget(target string, username string, password string, logger log.Logger) (Sansay, error) {
	var sansay Sansay

	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "http://" + target
	}

	_, err := url.Parse(target)
	if err != nil {
		level.Error(logger).Log("msg", "Could not parse target URL", "err", err)
		return sansay, err
	}
	client := &http.Client{}
	request, err := http.NewRequest("GET", target, http.NoBody)
	if err != nil {
		level.Error(logger).Log("msg", "Error creating HTTP request", "err", err)
		return sansay, err
	}

	request.SetBasicAuth(username, password)
	resp, err := client.Do(request)

	if err != nil {
		level.Error(logger).Log("msg", "Error for HTTP request", "err", err)
		return sansay, err
	}
	level.Info(logger).Log("msg", "Received HTTP response", "status_code", resp.StatusCode)
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		level.Info(logger).Log("msg", "Failed to read HTTP response body", "err", err)
		return sansay, err
	}
	err = xml.Unmarshal(body, &sansay)
	if err != nil {
		level.Error(logger).Log("msg", "Error parsing XML", "err", err)
		return sansay, err
	}
	return sansay, nil
}

func addMetric(ch chan<- prometheus.Metric, name string, value string) error {
	metricName := fmt.Sprintf("sansay_%s", name)
	floatValue, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return err
	}
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(metricName, "", nil, nil),
		prometheus.GaugeValue,
		floatValue)
	return nil
}
func addTrunkMetrics(ch chan<- prometheus.Metric, trunk Trunk) error {
	for _, metric := range []string{"NumOrig",
		"NumTerm",
		"Cps",
		"NumPeak",
		"TotalCLZ",
		"NumCLZCps",
		"TotalLimit",
		"CpsLimit"} {
		metricName := fmt.Sprintf("sansay_trunk_%s", strings.ToLower(metric))

		value, err := getField(&trunk, metric)
		if err != nil {
			ch <- prometheus.NewInvalidMetric(prometheus.NewDesc("sansay_error", "Error scraping target", nil, nil), err)
			continue
		}
		floatValue, err := strconv.ParseFloat(value, 64)
		if err != nil {
			ch <- prometheus.NewInvalidMetric(prometheus.NewDesc("sansay_error", "Error scraping target", nil, nil), err)
			continue
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(metricName, "", []string{"trunkgroup", "alias"}, nil),
			prometheus.GaugeValue,
			floatValue, trunk.TrunkId, trunk.Alias)
	}
	return nil
}

// setField sets field of v with given name to given value.
func setField(v interface{}, name string, value string) error {
	// v must be a pointer to a struct
	nme := []rune(name)
	nme[0] = unicode.ToUpper(nme[0])
	name = string(nme)
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr || rv.Elem().Kind() != reflect.Struct {
		return errors.New("v must be pointer to struct")
	}

	// Dereference pointer
	rv = rv.Elem()

	// Lookup field by name
	fv := rv.FieldByName(name)
	if !fv.IsValid() {
		return fmt.Errorf("not a field name: %s", name)
	}

	// Field must be exported
	if !fv.CanSet() {
		return fmt.Errorf("cannot set field %s", name)
	}

	// We expect a string field
	if fv.Kind() != reflect.String {
		return fmt.Errorf("%s is not a string field", name)
	}

	// Set the value
	fv.SetString(value)
	return nil
}

// setField sets field of v with given name to given value.
func getField(v interface{}, name string) (string, error) {
	// v must be a pointer to a struct
	nme := []rune(name)
	nme[0] = unicode.ToUpper(nme[0])
	name = string(nme)
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr || rv.Elem().Kind() != reflect.Struct {
		return "", errors.New("v must be pointer to struct")
	}

	// Dereference pointer
	rv = rv.Elem()

	// Lookup field by name
	fv := rv.FieldByName(name)
	if !fv.IsValid() {
		return "", fmt.Errorf("not a field name: %s", name)
	}

	// We expect a string field
	if fv.Kind() != reflect.String {
		return "", fmt.Errorf("%s is not a string field", name)
	}

	return fv.String(), nil
}