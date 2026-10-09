package panel

import (
	"net/http"
	"strings"
	"testing"
)

func TestConfigAcceptsPrioritySchedule(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)

	mustSave(t, p, `{"platforms":{"zcode":{"priority":5,"priority_schedule":[{"start":"01:00","end":"05:00","priority":1},{"start":"05:00","end":"08:00","priority":-2}]}}}`)

	platforms, ok := onDisk(t, path)["platforms"].(map[string]any)
	if !ok {
		t.Fatal("platforms block did not survive")
	}
	zcode, ok := platforms["zcode"].(map[string]any)
	if !ok {
		t.Fatal("zcode platform did not survive")
	}
	rules, ok := zcode["priority_schedule"].([]any)
	if !ok || len(rules) != 2 {
		t.Fatalf("priority_schedule = %#v, want two rules", zcode["priority_schedule"])
	}
	first, _ := rules[0].(map[string]any)
	if first["start"] != "01:00" || first["end"] != "05:00" || first["priority"] != float64(1) {
		t.Fatalf("first priority rule = %#v", first)
	}
}

func TestConfigRejectsBadPrioritySchedule(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "not an array",
			body: `{"platforms":{"zcode":{"priority_schedule":{"start":"01:00"}}}}`,
			want: "priority_schedule",
		},
		{
			name: "rule not an object",
			body: `{"platforms":{"zcode":{"priority_schedule":["01:00"]}}}`,
			want: "priority_schedule[0]",
		},
		{
			name: "start not a string",
			body: `{"platforms":{"zcode":{"priority_schedule":[{"start":1,"end":"05:00","priority":1}]}}}`,
			want: "priority_schedule[0].start",
		},
		{
			name: "bad clock",
			body: `{"platforms":{"zcode":{"priority_schedule":[{"start":"25:00","end":"05:00","priority":1}]}}}`,
			want: "priority_schedule[0].start",
		},
		{
			name: "same start and end",
			body: `{"platforms":{"zcode":{"priority_schedule":[{"start":"01:00","end":"01:00","priority":1}]}}}`,
			want: "must differ",
		},
		{
			name: "priority not an integer",
			body: `{"platforms":{"zcode":{"priority_schedule":[{"start":"01:00","end":"05:00","priority":1.5}]}}}`,
			want: "priority_schedule[0].priority",
		},
		{
			name: "overlap",
			body: `{"platforms":{"zcode":{"priority_schedule":[{"start":"01:00","end":"05:00","priority":1},{"start":"04:59","end":"08:00","priority":2}]}}}`,
			want: "overlap",
		},
		{
			name: "cross midnight overlap",
			body: `{"platforms":{"zcode":{"priority_schedule":[{"start":"23:00","end":"06:00","priority":1},{"start":"05:00","end":"07:00","priority":2}]}}}`,
			want: "overlap",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := configFile(t, baseConfig)
			before := onDisk(t, path)
			w, out := doConfig(t, configPanel(path), http.MethodPatch, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("PATCH = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			if msg, _ := out["error"].(string); !strings.Contains(msg, tc.want) {
				t.Fatalf("error %q does not mention %q", msg, tc.want)
			}
			if after := onDisk(t, path); !jsonEqual(before, after) {
				t.Fatalf("rejected schedule changed the file:\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}
