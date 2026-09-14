package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type renderedDiscord struct {
	Username string `json:"username"`
	Embeds   []struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Color       int    `json:"color"`
		Timestamp   string `json:"timestamp"`
		Footer      struct {
			Text string `json:"text"`
		} `json:"footer"`
		Fields []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"fields"`
	} `json:"embeds"`
	AllowedMentions struct {
		Parse []string `json:"parse"`
	} `json:"allowed_mentions"`
}

func renderForTest(t *testing.T, notificationType string, data string, serverName string) renderedDiscord {
	t.Helper()
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	body, err := renderDiscord(notification{
		Type: notificationType, ServerID: "srv-1", OccurredAt: at, LandedAt: at,
		Data: json.RawMessage(data),
	}, serverName)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var rendered renderedDiscord
	if err := json.Unmarshal(body, &rendered); err != nil {
		t.Fatalf("the rendered body is not JSON: %v\n%s", err, body)
	}
	if len(rendered.Embeds) != 1 {
		t.Fatalf("rendered %d embeds, want 1", len(rendered.Embeds))
	}
	if rendered.Embeds[0].Timestamp != "2026-09-14T12:00:00.000Z" {
		t.Errorf("embed timestamp = %q, want the notification's occurredAt", rendered.Embeds[0].Timestamp)
	}
	if rendered.AllowedMentions.Parse == nil || len(rendered.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed_mentions.parse = %v, want an empty list (section 11.3)", rendered.AllowedMentions.Parse)
	}
	return rendered
}

func TestDiscordDeathReadsLikeAKillFeed(t *testing.T) {
	t.Parallel()
	rendered := renderForTest(t, "core.player.death", `{
		"player": {"platform": "steam", "id": "1"}, "name": "Survivor",
		"killer": {"platform": "steam", "id": "2"}, "killerName": "Raider",
		"cause": "player", "weapon": "M4-A1", "distance": 312.4,
		"position": [4231.5, 300.2, 10620.0]}`, "Chernarus 1")
	embed := rendered.Embeds[0]
	if want := "**Survivor** was killed by **Raider** with M4-A1 from 312 m"; embed.Description != want {
		t.Errorf("description = %q, want %q", embed.Description, want)
	}
	if embed.Footer.Text != "Chernarus 1 · core.player.death" {
		t.Errorf("footer = %q, want the server name and the type", embed.Footer.Text)
	}
	if len(embed.Fields) != 1 || embed.Fields[0].Name != "Position" || embed.Fields[0].Value != "4232, 300, 10620" {
		t.Errorf("fields = %+v, want one Position field", embed.Fields)
	}

	self := renderForTest(t, "core.player.death", `{"name": "Survivor", "cause": "self"}`, "")
	if self.Embeds[0].Description != "**Survivor** died" {
		t.Errorf("self death = %q", self.Embeds[0].Description)
	}
	if self.Embeds[0].Footer.Text != "srv-1 · core.player.death" {
		t.Errorf("footer without a name = %q, want the server id", self.Embeds[0].Footer.Text)
	}
	unknown := renderForTest(t, "core.player.death", `{"player": {"platform": "steam", "id": "7"}}`, "")
	if unknown.Embeds[0].Description != "**steam:7** died" {
		t.Errorf("nameless death = %q", unknown.Embeds[0].Description)
	}
}

func TestDiscordEscapesPlayerText(t *testing.T) {
	t.Parallel()
	rendered := renderForTest(t, "core.player.chat", `{"name": "**@everyone**", "channel": "direct",
		"text": "join [here](https://example.net) _now_ `+"`code`"+` > quote "}`, "s")
	description := rendered.Embeds[0].Description
	for _, raw := range []string{"**@everyone**", "[here](", "_now_", "`code`", " > quote"} {
		if strings.Contains(description, raw) {
			t.Errorf("description %q still carries %q unescaped", description, raw)
		}
	}
	for _, escaped := range []string{`\*\*@everyone\*\*`, `\[here\]\(`, `\_now\_`, "\\`code\\`", `\> quote`} {
		if !strings.Contains(description, escaped) {
			t.Errorf("description %q lacks the escaped form %q", description, escaped)
		}
	}
	if rendered.Embeds[0].Title != "Chat (direct)" {
		t.Errorf("title = %q", rendered.Embeds[0].Title)
	}
}

func TestDiscordGenericEmbedForUnknownTypes(t *testing.T) {
	t.Parallel()
	rendered := renderForTest(t, "example-mod.raid.started",
		`{"territoryId": "t-19", "attackers": 4, "nested": {"a": [1, 2]}, "ok": true, "gone": null, "empty": ""}`, "s")
	embed := rendered.Embeds[0]
	if embed.Title != "example-mod.raid.started" {
		t.Errorf("title = %q, want the type", embed.Title)
	}
	got := map[string]string{}
	for _, field := range embed.Fields {
		got[field.Name] = field.Value
	}
	want := map[string]string{
		"territoryId": "t-19", "attackers": "4", "nested": `{"a":\[1,2\]}`, "ok": "true", "gone": "null", "empty": "(empty)",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("field %s = %q, want %q", key, got[key], value)
		}
	}
	if len(embed.Fields) != len(want) {
		t.Errorf("rendered %d fields, want %d", len(embed.Fields), len(want))
	}

	// More members than the generic embed shows: the rest is counted.
	many := "{"
	for i := 0; i < 14; i++ {
		if i > 0 {
			many += ","
		}
		many += `"k` + string(rune('a'+i)) + `": ` + string(rune('0'+i%10))
	}
	many += "}"
	rendered = renderForTest(t, "example-mod.many", many, "s")
	if n := len(rendered.Embeds[0].Fields); n != discordGenericFields+1 {
		t.Errorf("rendered %d fields for 14 members, want %d plus the remainder", n, discordGenericFields)
	}
	if last := rendered.Embeds[0].Fields[discordGenericFields]; last.Value != "4 more members not shown" {
		t.Errorf("remainder field = %+v", last)
	}

	empty := renderForTest(t, "example-mod.bare", `{}`, "s")
	if empty.Embeds[0].Description != "no payload" || len(empty.Embeds[0].Fields) != 0 {
		t.Errorf("empty payload rendered as %+v", empty.Embeds[0])
	}
}

func TestDiscordBoundsFieldLengths(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 9000)
	rendered := renderForTest(t, "core.player.chat", `{"name": "n", "channel": "direct", "text": "`+long+`"}`, strings.Repeat("s", 3000))
	embed := rendered.Embeds[0]
	if n := utf8.RuneCountInString(embed.Description); n > discordDescriptionMax {
		t.Errorf("description is %d runes, over %d", n, discordDescriptionMax)
	}
	if n := utf8.RuneCountInString(embed.Footer.Text); n > discordFooterMax {
		t.Errorf("footer is %d runes, over %d", n, discordFooterMax)
	}
	total := utf8.RuneCountInString(embed.Title) + utf8.RuneCountInString(embed.Description) + utf8.RuneCountInString(embed.Footer.Text)
	if total > discordEmbedTotalMax {
		t.Errorf("embed text totals %d runes, over %d", total, discordEmbedTotalMax)
	}
	if !strings.HasSuffix(embed.Description, "…") {
		t.Errorf("a cut description ends with an ellipsis; got %q", embed.Description[len(embed.Description)-8:])
	}
}

func TestDiscordLifecycleWording(t *testing.T) {
	t.Parallel()
	completed := renderForTest(t, notifyActionCompleted,
		`{"actionId": "01A", "code": "vyshka.heal", "state": "completed", "ok": true, "durationMs": 12}`, "s")
	if completed.Embeds[0].Description != "vyshka.heal completed in 12 ms" || completed.Embeds[0].Fields[0].Value != "01A" {
		t.Errorf("completed action rendered as %+v", completed.Embeds[0])
	}
	failed := renderForTest(t, notifyActionCompleted,
		`{"actionId": "01B", "code": "vyshka.kick", "state": "failed", "ok": false, "error": "player 1 is not online"}`, "s")
	if failed.Embeds[0].Description != "vyshka.kick failed: player 1 is not online" {
		t.Errorf("failed action rendered as %q", failed.Embeds[0].Description)
	}
	lost := renderForTest(t, notifyServerLinkLost, `{"lastSeenAt": "2026-09-14T11:59:00.000Z"}`, "s")
	if lost.Embeds[0].Title != "Server link lost" || !strings.Contains(lost.Embeds[0].Description, "2026-09-14T11:59:00.000Z") {
		t.Errorf("link lost rendered as %+v", lost.Embeds[0])
	}
	kick := renderForTest(t, "core.player.kick", `{"name": "Survivor", "reason": "afk", "cause": "ban", "actionId": "01C"}`, "s")
	if kick.Embeds[0].Description != "**Survivor** was refused: banned (afk)" {
		t.Errorf("ban refusal rendered as %q", kick.Embeds[0].Description)
	}
	fps := renderForTest(t, "core.server.fps", `{"fps": 42.55, "players": 1}`, "s")
	if fps.Embeds[0].Description != "42.5 fps, 1 player" && fps.Embeds[0].Description != "42.6 fps, 1 player" {
		t.Errorf("fps rendered as %q", fps.Embeds[0].Description)
	}
}
