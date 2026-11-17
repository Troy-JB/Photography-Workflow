package proof

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
)

// AnnotationSchema is bumped when coordinate or shape semantics change; stored
// annotations are rendered by the client that understands their schema.
const AnnotationSchema = "annotation.v1"

const (
	AnnotationPoint    = "point"
	AnnotationRect     = "rect"
	AnnotationEllipse  = "ellipse"
	AnnotationFreehand = "freehand"

	maxFreehandPoints = 256
)

var annotationColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}([0-9a-fA-F]{2})?$`)

// Point is a normalised position: 0,0 is the top-left of the displayed image
// and 1,1 the bottom-right, so annotations survive any preview size.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Annotation struct {
	Schema string  `json:"schema"`
	Type   string  `json:"type"`
	Points []Point `json:"points"`
	Color  string  `json:"color,omitempty"`
}

// ValidateAnnotation decodes and normalises an annotation payload. It returns a
// canonical re-encoding so the stored value is exactly what was validated, and
// rejects non-finite or out-of-image coordinates before they can reach the
// renderer.
func ValidateAnnotation(raw []byte, maxBytes int) (json.RawMessage, error) {
	if maxBytes <= 0 {
		maxBytes = 16384
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: annotation is empty", ErrInvalidAnnotation)
	}
	if len(raw) > maxBytes {
		return nil, fmt.Errorf("%w: annotation exceeds %d bytes", ErrInvalidAnnotation, maxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var a Annotation
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAnnotation, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data after annotation", ErrInvalidAnnotation)
	}
	if a.Schema != AnnotationSchema {
		return nil, fmt.Errorf("%w: schema must be %s", ErrInvalidAnnotation, AnnotationSchema)
	}
	switch a.Type {
	case AnnotationPoint:
		if len(a.Points) != 1 {
			return nil, fmt.Errorf("%w: point needs exactly 1 point", ErrInvalidAnnotation)
		}
	case AnnotationRect, AnnotationEllipse:
		if len(a.Points) != 2 {
			return nil, fmt.Errorf("%w: %s needs exactly 2 points", ErrInvalidAnnotation, a.Type)
		}
		if a.Points[0].X >= a.Points[1].X || a.Points[0].Y >= a.Points[1].Y {
			return nil, fmt.Errorf("%w: %s corners must satisfy x1<x2 and y1<y2", ErrInvalidAnnotation, a.Type)
		}
	case AnnotationFreehand:
		if len(a.Points) < 2 || len(a.Points) > maxFreehandPoints {
			return nil, fmt.Errorf("%w: freehand needs 2..%d points", ErrInvalidAnnotation, maxFreehandPoints)
		}
	default:
		return nil, fmt.Errorf("%w: unknown type %q", ErrInvalidAnnotation, a.Type)
	}
	for _, p := range a.Points {
		if math.IsNaN(p.X) || math.IsInf(p.X, 0) || math.IsNaN(p.Y) || math.IsInf(p.Y, 0) ||
			p.X < 0 || p.X > 1 || p.Y < 0 || p.Y > 1 {
			return nil, fmt.Errorf("%w: coordinates must be finite and within 0..1", ErrInvalidAnnotation)
		}
	}
	if a.Color != "" && !annotationColor.MatchString(a.Color) {
		return nil, fmt.Errorf("%w: color must be #RRGGBB or #RRGGBBAA", ErrInvalidAnnotation)
	}
	normalised, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAnnotation, err)
	}
	return normalised, nil
}
