// Package mail は専用 Gmail の読み取りを扱う。権限は gmail.readonly のみで、変更操作は持たない。
package mail

import (
	"context"
	"errors"
	"html"
	"net/http"
	"time"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/Tatsubooom/sheepbutler/internal/model"
	"github.com/Tatsubooom/sheepbutler/internal/source"
)

// ErrHistoryExpired は保存していた履歴 ID が古すぎて差分を取得できないことを示す。
// Gmail の履歴は通常1週間程度しか保持されない。
var ErrHistoryExpired = errors.New("gmail: 履歴 ID が期限切れです")

// InboxLabel は取り込み対象のラベル。
const InboxLabel = "INBOX"

// Gmail は Gmail API の読み取り操作をまとめたもの。
type Gmail struct {
	svc *gmail.Service
}

func NewGmail(ctx context.Context, hc *http.Client) (*Gmail, error) {
	svc, err := gmail.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, err
	}
	return &Gmail{svc: svc}, nil
}

// Profile は現在の履歴 ID を返す。
func (g *Gmail) Profile(ctx context.Context) (uint64, error) {
	p, err := g.svc.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		return 0, err
	}
	return p.HistoryId, nil
}

// History は startID 以降に受信トレイへ追加されたメールの ID と、最新の履歴 ID を返す。
func (g *Gmail) History(ctx context.Context, startID uint64) ([]string, uint64, error) {
	var ids []string
	seen := map[string]bool{}
	latest := startID
	err := g.svc.Users.History.List("me").
		StartHistoryId(startID).
		HistoryTypes("messageAdded").
		LabelId(InboxLabel).
		Pages(ctx, func(r *gmail.ListHistoryResponse) error {
			if r.HistoryId > latest {
				latest = r.HistoryId
			}
			for _, h := range r.History {
				for _, a := range h.MessagesAdded {
					if a.Message != nil && !seen[a.Message.Id] {
						seen[a.Message.Id] = true
						ids = append(ids, a.Message.Id)
					}
				}
			}
			return nil
		})
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Code == http.StatusNotFound {
		return nil, 0, ErrHistoryExpired
	}
	if err != nil {
		return nil, 0, err
	}
	return ids, latest, nil
}

// Recent は検索クエリに一致するメールの ID を新しい順に最大 max 件返す。履歴が期限切れのときの再同期に使う。
func (g *Gmail) Recent(ctx context.Context, query string, max int64) ([]string, error) {
	r, err := g.svc.Users.Messages.List("me").Q(query).MaxResults(max).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(r.Messages))
	for _, m := range r.Messages {
		ids = append(ids, m.Id)
	}
	return ids, nil
}

// Get はメールのヘッダーと冒頭の抜粋を取得する。本文は取得しない。
func (g *Gmail) Get(ctx context.Context, id string) (*model.Message, error) {
	headers := append([]string{"From", "Subject"}, source.Headers()...)
	m, err := g.svc.Users.Messages.Get("me", id).
		Format("metadata").
		MetadataHeaders(headers...).
		Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	msg := &model.Message{
		ID:         m.Id,
		ThreadID:   m.ThreadId,
		ReceivedAt: time.UnixMilli(m.InternalDate),
		// Gmail の snippet は HTML エスケープされている（&#39; など）。
		Snippet:  html.UnescapeString(m.Snippet),
		LabelIDs: m.LabelIds,
		Headers:  map[string][]string{},
	}
	if m.Payload != nil {
		for _, h := range m.Payload.Headers {
			msg.Headers[h.Name] = append(msg.Headers[h.Name], h.Value)
			switch h.Name {
			case "From":
				msg.From = h.Value
			case "To":
				msg.To = h.Value
			case "Subject":
				msg.Subject = h.Value
			}
		}
	}
	return msg, nil
}

// Watch は受信トレイの新着を Pub/Sub トピックへ通知するよう登録する。期限は約7日。
func (g *Gmail) Watch(ctx context.Context, topic string) (uint64, time.Time, error) {
	r, err := g.svc.Users.Watch("me", &gmail.WatchRequest{
		TopicName:           topic,
		LabelIds:            []string{InboxLabel},
		LabelFilterBehavior: "include",
	}).Context(ctx).Do()
	if err != nil {
		return 0, time.Time{}, err
	}
	return r.HistoryId, time.UnixMilli(r.Expiration), nil
}
