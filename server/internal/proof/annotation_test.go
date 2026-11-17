package proof

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateAnnotationAcceptsShapes(t *testing.T) {
	cases := map[string]string{
		"point":    `{"schema":"annotation.v1","type":"point","points":[{"x":0,"y":1}],"color":"#ff0000"}`,
		"rect":     `{"schema":"annotation.v1","type":"rect","points":[{"x":0.1,"y":0.2},{"x":0.4,"y":0.5}]}`,
		"ellipse":  `{"schema":"annotation.v1","type":"ellipse","points":[{"x":0,"y":0},{"x":1,"y":1}]}`,
		"freehand": `{"schema":"annotation.v1","type":"freehand","points":[{"x":0.1,"y":0.1},{"x":0.2,"y":0.3}],"color":"#00ff0080"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := ValidateAnnotation([]byte(raw), 16384)
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			var a Annotation
			if err := json.Unmarshal(out, &a); err != nil {
				t.Fatalf("re-parse: %v", err)
			}
			if a.Schema != AnnotationSchema || a.Type != name {
				t.Fatalf("normalised = %+v", a)
			}
		})
	}
}

func TestValidateAnnotationRejectsBadInput(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"empty", ``, "empty"},
		{"not json", `{`, "invalid"},
		{"unknown field", `{"schema":"annotation.v1","type":"point","points":[{"x":0,"y":0}],"zoom":2}`, "unknown field"},
		{"wrong schema", `{"schema":"annotation.v2","type":"point","points":[{"x":0,"y":0}]}`, "schema"},
		{"unknown type", `{"schema":"annotation.v1","type":"star","points":[{"x":0,"y":0}]}`, "unknown type"},
		{"point count", `{"schema":"annotation.v1","type":"point","points":[{"x":0,"y":0},{"x":1,"y":1}]}`, "exactly 1"},
		{"rect count", `{"schema":"annotation.v1","type":"rect","points":[{"x":0,"y":0}]}`, "exactly 2"},
		{"rect order", `{"schema":"annotation.v1","type":"rect","points":[{"x":0.5,"y":0.1},{"x":0.4,"y":0.6}]}`, "x1<x2"},
		{"ellipse order", `{"schema":"annotation.v1","type":"ellipse","points":[{"x":0.1,"y":0.6},{"x":0.4,"y":0.5}]}`, "y1<y2"},
		{"freehand short", `{"schema":"annotation.v1","type":"freehand","points":[{"x":0.1,"y":0.1}]}`, "2..256"},
		{"out of range", `{"schema":"annotation.v1","type":"point","points":[{"x":1.01,"y":0}]}`, "0..1"},
		{"negative", `{"schema":"annotation.v1","type":"point","points":[{"x":0,"y":-0.1}]}`, "0..1"},
		{"bad color", `{"schema":"annotation.v1","type":"point","points":[{"x":0,"y":0}],"color":"red"}`, "color"},
		{"trailing", `{"schema":"annotation.v1","type":"point","points":[{"x":0,"y":0}]}{}`, "trailing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ValidateAnnotation([]byte(c.raw), 16384); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want substring %q", err, c.want)
			}
		})
	}
}

func TestValidateAnnotationSizeCap(t *testing.T) {
	raw := `{"schema":"annotation.v1","type":"point","points":[{"x":0,"y":0}]}`
	if _, err := ValidateAnnotation([]byte(raw), len(raw)-1); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want size rejection", err)
	}
}

func TestValidateAnnotationFreehandCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"schema":"annotation.v1","type":"freehand","points":[`)
	for i := 0; i < maxFreehandPoints+1; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"x":0.5,"y":0.5}`)
	}
	b.WriteString(`]}`)
	if _, err := ValidateAnnotation([]byte(b.String()), 1<<20); err == nil || !strings.Contains(err.Error(), "2..256") {
		t.Fatalf("error = %v, want point cap rejection", err)
	}
}
