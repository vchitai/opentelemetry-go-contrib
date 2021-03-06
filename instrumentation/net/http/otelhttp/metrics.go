// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package otelhttp

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/semconv"

	"go.opentelemetry.io/otel/unit"

	"go.opentelemetry.io/otel/api/metric"
	"go.opentelemetry.io/otel/label"
)

type instrumentedTransport struct {
	meter                  metric.Meter
	base                   *Transport
	clientDurationRecorder metric.Float64ValueRecorder
}

type tracker struct {
	ctx     context.Context
	start   time.Time
	body    io.ReadCloser
	endOnce sync.Once
	labels  []label.KeyValue

	clientDurationRecorder metric.Float64ValueRecorder
}

func (trans *instrumentedTransport) applyConfig(c *config) {
	trans.base.applyConfig(c)

	trans.meter = c.Meter
	trans.createMeasures()
}

// RoundTrip implements http.RoundTripper, delegating to Base and recording stats for the request.
func (trans *instrumentedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	labels := semconv.HTTPClientAttributesFromHTTPRequest(req)

	ctx := req.Context()
	tracker := &tracker{
		start:                  time.Now(),
		ctx:                    ctx,
		clientDurationRecorder: trans.clientDurationRecorder,
	}

	resp, err := trans.base.RoundTrip(req)
	if err != nil {
		tracker.labels = append(labels, semconv.HTTPAttributesFromHTTPStatusCode(http.StatusInternalServerError)...)
		tracker.end()
	} else {
		tracker.labels = append(labels, semconv.HTTPAttributesFromHTTPStatusCode(resp.StatusCode)...)
		if resp.Body == nil {
			tracker.end()
		} else {
			tracker.body = resp.Body
			resp.Body = wrappedBodyIO(tracker, resp.Body)
		}
	}
	return resp, err
}

// wrappedBodyIO returns a wrapped version of the original
// Body and only implements the same combination of additional
// interfaces as the original.
func wrappedBodyIO(wrapper io.ReadCloser, body io.ReadCloser) io.ReadCloser {
	if wr, ok := body.(io.Writer); ok {
		return struct {
			io.ReadCloser
			io.Writer
		}{wrapper, wr}
	}
	return wrapper
}

func (trans *instrumentedTransport) createMeasures() {
	var err error
	trans.clientDurationRecorder, err = trans.meter.NewFloat64ValueRecorder(
		clientRequestDuration,
		metric.WithDescription("measures the duration of the outbound HTTP request"),
		metric.WithUnit(unit.Milliseconds),
	)
	handleErr(err)
}

var _ io.ReadCloser = (*tracker)(nil)

func (tracker *tracker) end() {
	tracker.endOnce.Do(func() {
		latencyMs := float64(time.Since(tracker.start)) / float64(time.Millisecond)
		tracker.clientDurationRecorder.Record(tracker.ctx, latencyMs, tracker.labels...)
	})
}

func (tracker *tracker) Read(b []byte) (int, error) {
	n, err := tracker.body.Read(b)
	switch err {
	case nil:
		return n, nil
	case io.EOF:
		tracker.end()
	}
	return n, err
}

func (tracker *tracker) Close() error {
	tracker.end()
	return tracker.body.Close()
}
