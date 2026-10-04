// Package logx は Cloud Logging が解釈できる JSON 形式のロガーを用意する。
// メールの件名や本文はログに出さない方針のため、ログには ID と件数だけを渡すこと。
package logx

import (
	"io"
	"log/slog"
)

// New は level を severity、msg を message として出力する slog ロガーを返す。
func New(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.LevelKey:
				a.Key = "severity"
			case slog.MessageKey:
				a.Key = "message"
			}
			return a
		},
	}))
}
