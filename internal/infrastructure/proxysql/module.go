package proxysql

import (
	"context"
	"strconv"
	"sync"

	"github.com/db-branch/db-branch/pkg/ext"
)

// Settings are the proxysql module's settings in the console.
var Settings = []ext.Setting{
	{Key: "comment_routing", Label: "Route by query comment", Type: "bool", Default: "true",
		Help:         "/*db_branch='name'*/ in a query picks the branch. Off: connections reach their user's branch only.",
		Translations: ja("クエリのコメントで振り分ける", "クエリ中の /*db_branch='name'*/ で接続先のブランチを選びます。オフにすると、接続はそのユーザーのブランチだけに届きます。")},
	{Key: "server_version", Label: "Reported MySQL version", Type: "string", Default: "8.4.0",
		Help:         "What clients see as the server version.",
		Translations: ja("通知する MySQL のバージョン", "クライアントから見えるサーバーのバージョンです。")},
}

func ja(label, help string) map[string]ext.SettingText {
	return map[string]ext.SettingText{"ja": {Label: label, Help: help}}
}

// Module is the proxysql module: a Router whose behaviour the settings change.
// A settings change re-applies the last routing so it takes effect at once.
type Module struct {
	admin *Admin
	mu    sync.Mutex
	last  *ext.RouteConfig
}

// NewModule wraps an Admin.
func NewModule(a *Admin) *Module { return &Module{admin: a} }

// Route applies c and remembers it.
func (m *Module) Route(ctx context.Context, c ext.RouteConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = &c
	return m.admin.Apply(ctx, c)
}

// Configure takes the settings and re-applies the last routing.
func (m *Module) Configure(ctx context.Context, values map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	routing, err := strconv.ParseBool(orDefault(values["comment_routing"], "true"))
	if err != nil {
		return err
	}
	m.admin.Options = Options{NoCommentRouting: !routing, ServerVersion: orDefault(values["server_version"], "8.4.0")}
	if m.last == nil {
		return nil
	}
	return m.admin.Apply(ctx, *m.last)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
