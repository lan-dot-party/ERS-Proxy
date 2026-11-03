package web

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/store"
)

// Chart geometry (SVG user units). The SVG scales to the page width.
const (
	chartW, chartH       = 900.0, 300.0
	chartX0, chartX1     = 70.0, 886.0 // plot area left/right
	chartYBot, chartYTop = 262.0, 14.0 // plot area bottom/top
	errH                 = 64.0
)

// Spans offered on the port page.
var chartSpans = []struct {
	Key string
	D   time.Duration
}{
	{"1h", time.Hour}, {"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour},
	{"30d", 30 * 24 * time.Hour}, {"1y", 365 * 24 * time.Hour},
}

const defaultSpan = "24h"

func spanDuration(key string) (string, time.Duration) {
	for _, s := range chartSpans {
		if s.Key == key {
			return s.Key, s.D
		}
	}
	return defaultSpan, 24 * time.Hour
}

// fmtBps formats a rate with a human unit.
func fmtBps(v float64) string {
	switch {
	case v >= 1e9:
		return strconv.FormatFloat(v/1e9, 'f', 2, 64) + " Gbit/s"
	case v >= 1e6:
		return strconv.FormatFloat(v/1e6, 'f', 1, 64) + " Mbit/s"
	case v >= 1e3:
		return strconv.FormatFloat(v/1e3, 'f', 0, 64) + " kbit/s"
	}
	return strconv.FormatFloat(v, 'f', 0, 64) + " bit/s"
}

// axisBps is fmtBps without decimals noise for axis labels.
func axisBps(v float64) string {
	switch {
	case v >= 1e9:
		return trimZero(v/1e9) + " Gbit/s"
	case v >= 1e6:
		return trimZero(v/1e6) + " Mbit/s"
	case v >= 1e3:
		return trimZero(v/1e3) + " kbit/s"
	}
	return trimZero(v) + " bit/s"
}

func trimZero(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

// niceCeil rounds up to 1, 2, 2.5, 5 or 10 times a power of ten.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	e := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if v <= m*e {
			return m * e
		}
	}
	return 10 * e
}

type chartData struct {
	SVG    template.HTML // main traffic chart
	ErrSVG template.HTML // errors per bucket
	Max    float64       // highest rate in the chart
	Errors int64
}

// buildChart renders the traffic chart server-side. All numbers come from Go;
// nothing a device reports is part of the markup.
func buildChart(samples []store.Sample, span time.Duration, now time.Time, label string) chartData {
	if len(samples) == 0 {
		return chartData{}
	}
	t1 := now.Unix()
	t0 := t1 - int64(span/time.Second)
	res := int64(samples[0].Res)
	if res <= 0 {
		res = 60
	}
	showMax := res > 60

	peak := 0.0
	var errMax int64
	var errTotal int64
	for _, s := range samples {
		peak = math.Max(peak, math.Max(s.InAvg, s.OutAvg))
		if showMax {
			peak = math.Max(peak, math.Max(s.InMax, s.OutMax))
		}
		if e := s.InErrors + s.OutErrors; e > errMax {
			errMax = e
		}
		errTotal += s.InErrors + s.OutErrors
	}
	top := niceCeil(math.Max(peak, 1000))
	x := func(ts int64) float64 {
		return chartX0 + (chartX1-chartX0)*float64(ts-t0)/float64(t1-t0)
	}
	y := func(v float64) float64 { return chartYBot - (chartYBot-chartYTop)*v/top }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %g %g" role="img" aria-label="%s" data-t0="%d" data-t1="%d" data-res="%d" data-top="%s"`,
		chartW, chartH, template.HTMLEscapeString(label), t0, t1, res, trimZero(top))
	b.WriteString(` data-ts="`)
	join(&b, samples, func(s store.Sample) string { return strconv.FormatInt(s.TS.Unix(), 10) })
	for _, c := range []struct {
		name string
		f    func(store.Sample) float64
	}{
		{"in", func(s store.Sample) float64 { return s.InAvg }},
		{"out", func(s store.Sample) float64 { return s.OutAvg }},
		{"inmax", func(s store.Sample) float64 { return s.InMax }},
		{"outmax", func(s store.Sample) float64 { return s.OutMax }},
	} {
		fmt.Fprintf(&b, `" data-%s="`, c.name)
		join(&b, samples, func(s store.Sample) string { return strconv.FormatFloat(c.f(s), 'f', 0, 64) })
	}
	b.WriteString(`" data-err="`)
	join(&b, samples, func(s store.Sample) string { return strconv.FormatInt(s.InErrors+s.OutErrors, 10) })
	b.WriteString(`" data-up="`)
	join(&b, samples, func(s store.Sample) string { return strconv.FormatFloat(s.UpFrac, 'f', 2, 64) })
	b.WriteString(`">`)

	// Horizontal grid and y labels.
	for i := 0; i <= 4; i++ {
		v := top * float64(i) / 4
		fmt.Fprintf(&b, `<line class="grid" x1="%g" x2="%g" y1="%.1f" y2="%.1f"/>`, chartX0, chartX1, y(v), y(v))
		fmt.Fprintf(&b, `<text class="axis" x="%g" y="%.1f" text-anchor="end">%s</text>`, chartX0-6, y(v)+4, axisBps(v))
	}
	// X ticks: six evenly spaced labels; the browser rewrites them in its time zone.
	format := "time"
	if span > 36*time.Hour {
		format = "date"
	}
	for i := 0; i <= 5; i++ {
		ts := t0 + (t1-t0)*int64(i)/5
		anchor := "middle"
		if i == 0 {
			anchor = "start"
		} else if i == 5 {
			anchor = "end"
		}
		fmt.Fprintf(&b, `<line class="grid" x1="%.1f" x2="%.1f" y1="%g" y2="%g"/>`, x(ts), x(ts), chartYBot, chartYBot+4)
		fmt.Fprintf(&b, `<text class="axis xl" data-ts="%d" data-fmt="%s" x="%.1f" y="%g" text-anchor="%s">%s</text>`,
			ts, format, x(ts), chartYBot+18, anchor, fmtAxisTime(time.Unix(ts, 0), format))
	}

	// Down periods as a faint band.
	bw := math.Max(1, (chartX1-chartX0)*float64(res)/float64(t1-t0))
	for _, s := range samples {
		if s.UpFrac < 0.5 {
			fmt.Fprintf(&b, `<rect class="down-band" x="%.1f" y="%g" width="%.1f" height="%g"/>`, x(s.TS.Unix()), chartYTop, bw, chartYBot-chartYTop)
		}
	}

	line := func(cls string, f func(store.Sample) float64) {
		var d strings.Builder
		var dots strings.Builder
		prev := int64(-1)
		segLen := 0
		var lastX, lastY float64
		flush := func() {
			if segLen == 1 {
				fmt.Fprintf(&dots, `<circle class="%s-dot" cx="%.1f" cy="%.1f" r="2"/>`, cls, lastX, lastY)
			}
		}
		for _, s := range samples {
			ts := s.TS.Unix()
			px, py := x(ts), y(f(s))
			if prev < 0 || ts-prev > res*5/2 {
				flush()
				segLen = 0
				fmt.Fprintf(&d, "M%.1f %.1f", px, py)
			} else {
				fmt.Fprintf(&d, "L%.1f %.1f", px, py)
			}
			segLen++
			lastX, lastY, prev = px, py, ts
		}
		flush()
		fmt.Fprintf(&b, `<path class="%s" d="%s"/>%s`, cls, d.String(), dots.String())
	}
	if showMax {
		line("line-in-max", func(s store.Sample) float64 { return s.InMax })
		line("line-out-max", func(s store.Sample) float64 { return s.OutMax })
	}
	line("line-in", func(s store.Sample) float64 { return s.InAvg })
	line("line-out", func(s store.Sample) float64 { return s.OutAvg })
	b.WriteString(`</svg>`)

	// Errors per bucket.
	var e strings.Builder
	fmt.Fprintf(&e, `<svg class="chart chart-err" viewBox="0 0 %g %g" role="img" aria-label="%s">`, chartW, errH, template.HTMLEscapeString(label))
	fmt.Fprintf(&e, `<line class="grid" x1="%g" x2="%g" y1="%g" y2="%g"/>`, chartX0, chartX1, errH-8, errH-8)
	fmt.Fprintf(&e, `<text class="axis" x="%g" y="%g" text-anchor="end">%d</text>`, chartX0-6, 14.0, errMax)
	fmt.Fprintf(&e, `<text class="axis" x="%g" y="%g" text-anchor="end">0</text>`, chartX0-6, errH-4)
	for _, s := range samples {
		n := s.InErrors + s.OutErrors
		if n <= 0 {
			continue
		}
		h := math.Max(2, (errH-8-6)*float64(n)/float64(errMax))
		fmt.Fprintf(&e, `<rect class="err-bar" x="%.1f" y="%.1f" width="%.1f" height="%.1f"><title>%d</title></rect>`,
			x(s.TS.Unix()), errH-8-h, bw, h, n)
	}
	e.WriteString(`</svg>`)

	return chartData{SVG: template.HTML(b.String()), ErrSVG: template.HTML(e.String()), Max: peak, Errors: errTotal} //nolint:gosec // numbers only
}

func join(b *strings.Builder, samples []store.Sample, f func(store.Sample) string) {
	for i, s := range samples {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(f(s))
	}
}

func fmtAxisTime(t time.Time, format string) string {
	if format == "date" {
		return t.Format("02.01. 15:04")
	}
	return t.Format("15:04")
}
