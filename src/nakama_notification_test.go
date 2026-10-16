package main

import (
	"encoding/json"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// A realtime "notifications" message as Nakama 3.41 sends it: the content is
// JSON text (Nakama writes it with ", " and ": "), the code a number.
const nakamaNotificationsEnvelope = `{"notifications":{"notifications":[
 {"id":"9b9b6915-7a0a-4401-90eb-53a94cf22b9e","subject":"ikemen_ranked_settled",
  "content":"{\"change\": 512, \"change_text\": \"+512\", \"label\": \"Power level\", \"level\": 9000000000000000, \"level_before\": 8999999999999488, \"level_before_text\": \"8,999,999,999,999,488\", \"level_text\": \"9,000,000,000,000,000\", \"match_id\": \"a05f6f6b.lv\", \"rating\": 1016, \"won\": true}",
  "code":4101,"create_time":"2026-10-16T00:00:11Z","persistent":true},
 {"id":"2","subject":"ikemen_ranked_disputed","content":"{\"match_id\": \"a05f6f6b.lv#2\", \"reason\": \"claims_disagree\"}",
  "code":"4102","sender_id":"","create_time":"2026-10-16T00:00:12Z","persistent":true},
 {"id":"3","subject":"something_else","content":"not json","code":7}
]}}`

func TestParseNakamaNotifications(t *testing.T) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(nakamaNotificationsEnvelope), &raw); err != nil {
		t.Fatal(err)
	}
	list, err := parseNakamaNotifications(raw["notifications"])
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("%d notifications", len(list))
	}
	settled := list[0]
	if settled.Subject != "ikemen_ranked_settled" || settled.Code != 4101 || !settled.Persistent || settled.ID == "" {
		t.Fatalf("settled %+v", settled)
	}
	// Large numbers stay exact (json.Number), and the texts are the server's.
	if got := settled.Content["level"]; got != json.Number("9000000000000000") {
		t.Fatalf("level %v (%T)", got, got)
	}
	if got := settled.Content["level_before"]; got != json.Number("8999999999999488") {
		t.Fatalf("level_before %v", got)
	}
	if settled.Content["level_text"] != "9,000,000,000,000,000" || settled.Content["change_text"] != "+512" || settled.Content["won"] != true {
		t.Fatalf("content %v", settled.Content)
	}
	// A code sent as a string (protojson's int64 form) reads the same.
	if list[1].Code != 4102 || list[1].Content["reason"] != "claims_disagree" {
		t.Fatalf("disputed %+v", list[1])
	}
	// Content that is not a JSON object stays text.
	if list[2].Content != nil || list[2].ContentText != "not json" {
		t.Fatalf("text content %+v", list[2])
	}
	if p := list[2].luaPayload(); p["content"] != "not json" || p["code"] != int64(7) {
		t.Fatalf("payload %v", p)
	}
	if _, err := parseNakamaNotifications(json.RawMessage(`{"notifications": 5}`)); err == nil {
		t.Fatal("a malformed message was read")
	}
	if list, err := parseNakamaNotifications(json.RawMessage(`{}`)); err != nil || len(list) != 0 {
		t.Fatalf("an empty message: %v %v", list, err)
	}
}

// The client emits one "notification" event per notification, whose payload
// reaches Lua with the content as a table and its numbers exact.
func TestNakamaNotificationEvents(t *testing.T) {
	n := NewNakamaClient(NakamaConfig{Host: "127.0.0.1"})
	var events []NakamaEvent
	n.SetEventHandler(func(ev NakamaEvent) { events = append(events, ev) })
	n.handleJSON([]byte(nakamaNotificationsEnvelope))
	if len(events) != 3 {
		t.Fatalf("%d events", len(events))
	}
	for i, subject := range []string{"ikemen_ranked_settled", "ikemen_ranked_disputed", "something_else"} {
		p, _ := events[i].Payload.(map[string]any)
		if events[i].Type != "notification" || p["subject"] != subject {
			t.Fatalf("event %d: %s %v", i, events[i].Type, p)
		}
	}

	l := lua.NewState()
	defer l.Close()
	l.SetGlobal("ev", toLValue(l, events[0].Payload))
	if err := l.DoString(`
		local c = ev.content
		assert(ev.subject == "ikemen_ranked_settled", "subject")
		assert(ev.code == 4101, "code")
		assert(ev.persistent == true, "persistent")
		assert(type(c) == "table", "content")
		assert(c.level == 9000000000000000, "level " .. tostring(c.level))
		assert(c.level - c.level_before == c.change, "change")
		assert(c.level_text == "9,000,000,000,000,000", "level_text")
		assert(c.won == true and c.label == "Power level" and c.match_id == "a05f6f6b.lv", "fields")
	`); err != nil {
		t.Fatal(err)
	}

	// A message the client cannot read is not the client's error.
	events = nil
	n.handleJSON([]byte(`{"notifications":{"notifications":"x"}}`))
	if len(events) != 0 || n.Status().LastError != "" {
		t.Fatalf("events %v, error %q", events, n.Status().LastError)
	}
}

// Events reach Lua handlers with the payload as data.payload, one call per
// notification, in the order sent.
func TestNakamaNotificationLuaHandler(t *testing.T) {
	l := lua.NewState()
	defer l.Close()
	callbacks := newNakamaLuaHandlers()
	if err := l.DoString(`got = {}
		function on_notification(event, data)
			got[#got + 1] = event .. " " .. data.payload.subject .. " " .. tostring(data.payload.content.level_text or data.payload.content.reason or data.payload.content)
		end`); err != nil {
		t.Fatal(err)
	}
	callbacks.set("notification", "test", l.GetGlobal("on_notification").(*lua.LFunction))
	n := NewNakamaClient(NakamaConfig{Host: "127.0.0.1"})
	n.SetEventHandler(func(ev NakamaEvent) { nakamaDispatchLuaEvent(l, callbacks, ev) })
	n.handleJSON([]byte(nakamaNotificationsEnvelope))
	got := l.GetGlobal("got").(*lua.LTable)
	want := []string{
		"notification ikemen_ranked_settled 9,000,000,000,000,000",
		"notification ikemen_ranked_disputed claims_disagree",
		"notification something_else not json",
	}
	if got.Len() != len(want) {
		t.Fatalf("%d handler calls", got.Len())
	}
	for i, w := range want {
		if s := got.RawGetInt(i + 1).String(); s != w {
			t.Fatalf("call %d: %q, want %q", i+1, s, w)
		}
	}
}
