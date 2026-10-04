package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/mail"
	"github.com/Tatsubooom/sheepbutler/internal/model"
)

type fakeMailbox struct {
	profileID  uint64
	historyIDs []string
	historyTo  uint64
	historyErr error
	recentIDs  []string
	getErr     map[string]error
	gets       int
	watchID    uint64
	watchExp   time.Time
}

func (f *fakeMailbox) Profile(context.Context) (uint64, error) { return f.profileID, nil }
func (f *fakeMailbox) History(_ context.Context, start uint64) ([]string, uint64, error) {
	if f.historyErr != nil {
		return nil, 0, f.historyErr
	}
	return f.historyIDs, f.historyTo, nil
}
func (f *fakeMailbox) Recent(context.Context, string, int64) ([]string, error) {
	return f.recentIDs, nil
}
func (f *fakeMailbox) Get(_ context.Context, id string) (*model.Message, error) {
	f.gets++
	if err := f.getErr[id]; err != nil {
		return nil, err
	}
	return &model.Message{ID: id, Subject: "件名 " + id, ReceivedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}, nil
}
func (f *fakeMailbox) Watch(context.Context, string) (uint64, time.Time, error) {
	return f.watchID, f.watchExp, nil
}

type fakeStore struct {
	state    model.GmailState
	messages map[string]*model.Message
}

func newFakeStore(historyID uint64) *fakeStore {
	return &fakeStore{state: model.GmailState{HistoryID: historyID}, messages: map[string]*model.Message{}}
}

func (f *fakeStore) GmailState(context.Context) (model.GmailState, error) { return f.state, nil }
func (f *fakeStore) AdvanceHistoryID(_ context.Context, id uint64) error {
	if id > f.state.HistoryID {
		f.state.HistoryID = id
	}
	return nil
}
func (f *fakeStore) SetWatchExpiration(_ context.Context, exp time.Time) error {
	f.state.WatchExpiresAt = exp
	return nil
}
func (f *fakeStore) MessageExists(_ context.Context, id string) (bool, error) {
	_, ok := f.messages[id]
	return ok, nil
}
func (f *fakeStore) CreateMessage(_ context.Context, m *model.Message) (bool, error) {
	if _, ok := f.messages[m.ID]; ok {
		return false, nil
	}
	f.messages[m.ID] = m
	return true, nil
}

var now = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func newSyncer(mb *fakeMailbox, st *fakeStore) *Syncer {
	return &Syncer{Mailbox: mb, Store: st, Retention: 90 * 24 * time.Hour, Now: func() time.Time { return now }}
}

func TestSyncFirstRunOnlyRecordsHistoryID(t *testing.T) {
	mb := &fakeMailbox{profileID: 500, historyIDs: []string{"a"}}
	st := newFakeStore(0)

	res, err := newSyncer(mb, st).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Initialized || st.state.HistoryID != 500 {
		t.Fatalf("初回は履歴 ID の記録だけ行う: res=%+v historyID=%d", res, st.state.HistoryID)
	}
	if len(st.messages) != 0 || mb.gets != 0 {
		t.Fatal("初回は過去のメールを取り込まない")
	}
}

func TestSyncIngestsNewMessages(t *testing.T) {
	mb := &fakeMailbox{historyIDs: []string{"a", "b"}, historyTo: 120}
	st := newFakeStore(100)

	res, err := newSyncer(mb, st).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 2 || st.state.HistoryID != 120 {
		t.Fatalf("res=%+v historyID=%d", res, st.state.HistoryID)
	}
	m := st.messages["a"]
	if m.Status != model.StatusNew || !m.IngestedAt.Equal(now) {
		t.Errorf("状態と取り込み時刻: %+v", m)
	}
	if want := m.ReceivedAt.Add(90 * 24 * time.Hour); !m.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v（受信日時＋保存期間）", m.ExpiresAt, want)
	}
}

func TestSyncSkipsAlreadyIngested(t *testing.T) {
	mb := &fakeMailbox{historyIDs: []string{"a", "b"}, historyTo: 120}
	st := newFakeStore(100)
	st.messages["a"] = &model.Message{ID: "a"}

	res, err := newSyncer(mb, st).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 1 || res.Skipped != 1 {
		t.Fatalf("res=%+v", res)
	}
	if mb.gets != 1 {
		t.Errorf("取り込み済みのメールは Gmail から取得しない: gets=%d", mb.gets)
	}
}

func TestSyncResyncsWhenHistoryExpired(t *testing.T) {
	mb := &fakeMailbox{historyErr: mail.ErrHistoryExpired, profileID: 900, recentIDs: []string{"x", "y"}}
	st := newFakeStore(100)
	st.messages["x"] = &model.Message{ID: "x"}

	res, err := newSyncer(mb, st).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Resynced || res.Created != 1 || res.Skipped != 1 || st.state.HistoryID != 900 {
		t.Fatalf("res=%+v historyID=%d", res, st.state.HistoryID)
	}
}

func TestSyncDoesNotAdvanceOnFailure(t *testing.T) {
	mb := &fakeMailbox{
		historyIDs: []string{"a", "b", "c"},
		historyTo:  120,
		getErr:     map[string]error{"b": errors.New("一時的なエラー")},
	}
	st := newFakeStore(100)
	s := newSyncer(mb, st)

	if _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("取得に失敗したらエラーを返す")
	}
	if st.state.HistoryID != 100 {
		t.Fatalf("失敗時は履歴 ID を進めない: %d", st.state.HistoryID)
	}

	// 再送されたときは、取り込み済みの a を飛ばして続きから処理する。
	delete(mb.getErr, "b")
	res, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 2 || res.Skipped != 1 || st.state.HistoryID != 120 {
		t.Fatalf("res=%+v historyID=%d", res, st.state.HistoryID)
	}
}

func TestRenewWatch(t *testing.T) {
	exp := now.Add(7 * 24 * time.Hour)

	t.Run("初回は watch の履歴 ID を起点にする", func(t *testing.T) {
		st := newFakeStore(0)
		got, err := newSyncer(&fakeMailbox{watchID: 300, watchExp: exp}, st).RenewWatch(context.Background(), "projects/p/topics/t")
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(exp) || st.state.HistoryID != 300 || !st.state.WatchExpiresAt.Equal(exp) {
			t.Fatalf("exp=%v state=%+v", got, st.state)
		}
	})

	t.Run("既に同期中なら履歴 ID は変えない", func(t *testing.T) {
		st := newFakeStore(400)
		if _, err := newSyncer(&fakeMailbox{watchID: 300, watchExp: exp}, st).RenewWatch(context.Background(), "projects/p/topics/t"); err != nil {
			t.Fatal(err)
		}
		if st.state.HistoryID != 400 {
			t.Fatalf("historyID=%d", st.state.HistoryID)
		}
	})
}
