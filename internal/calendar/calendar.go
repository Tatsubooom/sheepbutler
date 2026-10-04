// Package calendar は Google カレンダーの予定を読み取る（calendar.readonly のみ）。
package calendar

import (
	"context"
	"net/http"
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
	id  string
	loc *time.Location
}

// NewGoogle は calendarID のカレンダーを読むクライアントを返す。
// メインアカウントのカレンダーを専用 Gmail に共有しておく必要がある。
func NewGoogle(ctx context.Context, hc *http.Client, calendarID string, loc *time.Location) (*Google, error) {
	svc, err := gcal.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, err
	}
	return &Google{svc: svc, id: calendarID, loc: loc}, nil
}

// Events は [from, to) に重なる予定を開始時刻の順に返す。
func (g *Google) Events(ctx context.Context, from, to time.Time) ([]Event, error) {
	res, err := g.svc.Events.List(g.id).
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
