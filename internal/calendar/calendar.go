// Package calendar は Google カレンダーの予定を読み取る（calendar.readonly のみ）。
package calendar

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	gcal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

type Event struct {
	Summary  string
	Location string
	Start    time.Time
	End      time.Time
	AllDay   bool
}

type Google struct {
	svc *gcal.Service
	ids []string
	loc *time.Location
}

// NewGoogle はカレンダーを読むクライアントを返す。calendarIDs はカンマ区切りで複数指定できる
// （例: メインの Gmail と大学のカレンダー）。各カレンダーを専用 Gmail に共有しておく必要がある。
func NewGoogle(ctx context.Context, hc *http.Client, calendarIDs string, loc *time.Location) (*Google, error) {
	var ids []string
	for _, id := range strings.FieldsFunc(calendarIDs, func(r rune) bool { return r == ',' || r == ';' }) {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("カレンダーの ID が空です")
	}
	svc, err := gcal.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, err
	}
	return &Google{svc: svc, ids: ids, loc: loc}, nil
}

// Events は [from, to) に重なる予定を、すべてのカレンダーからまとめて開始時刻の順に返す。
// 同じ予定が複数のカレンダーにある（招待など）ときは1つにする。
// 一部のカレンダーだけ読めないときは、読めた分を返す（すべて読めなければエラー）。
func (g *Google) Events(ctx context.Context, from, to time.Time) ([]Event, error) {
	var out []Event
	var firstErr error
	ok := 0
	seen := map[string]bool{}
	for _, id := range g.ids {
		evs, err := g.events(ctx, id, from, to)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok++
		for _, e := range evs {
			key := e.Summary + "|" + e.Start.UTC().Format(time.RFC3339)
			if !seen[key] {
				seen[key] = true
				out = append(out, e)
			}
		}
	}
	if ok == 0 {
		return nil, firstErr
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

func (g *Google) events(ctx context.Context, id string, from, to time.Time) ([]Event, error) {
	res, err := g.svc.Events.List(id).
		TimeMin(from.Format(time.RFC3339)).
		TimeMax(to.Format(time.RFC3339)).
		SingleEvents(true).
		OrderBy("startTime").
		MaxResults(50).
		Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(res.Items))
	for _, it := range res.Items {
		if it.Status == "cancelled" || it.Start == nil {
			continue
		}
		ev := Event{Summary: it.Summary, Location: it.Location}
		if it.Start.Date != "" { // 終日の予定
			ev.AllDay = true
			ev.Start, _ = time.ParseInLocation("2006-01-02", it.Start.Date, g.loc)
			if it.End != nil {
				ev.End, _ = time.ParseInLocation("2006-01-02", it.End.Date, g.loc)
			}
		} else {
			ev.Start, _ = time.Parse(time.RFC3339, it.Start.DateTime)
			if it.End != nil {
				ev.End, _ = time.Parse(time.RFC3339, it.End.DateTime)
			}
		}
		out = append(out, ev)
	}
	return out, nil
}
