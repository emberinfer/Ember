package gateway

import (
	"ember/services/jsonspool"
	"ember/services/verifier"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

type invocationJSON struct {
	Request  string  `json:"request"`
	Response string  `json:"response"`
	Profile  string  `json:"profile"`
	StageMS  float64 `json:"stage_ms"`
}

func (e *PipelineExecution) InvocationCount() int {
	if e.Source != nil {
		return len(e.Source.Arrays["invocations"])
	}
	return len(e.Invocations)
}
func (e *PipelineExecution) ReadInvocation(i int) (verifier.PipelineInvocation, error) {
	if e.Source == nil {
		return e.Invocations[i], nil
	}
	var v invocationJSON
	if err := e.Source.DecodeSpan(e.Source.Arrays["invocations"][i], &v); err != nil {
		return verifier.PipelineInvocation{}, err
	}
	a, ea := hex.DecodeString(v.Request)
	b, eb := hex.DecodeString(v.Response)
	c, ec := hex.DecodeString(v.Profile)
	if ea != nil || eb != nil || ec != nil {
		return verifier.PipelineInvocation{}, errors.New("invalid pipeline invocation encoding")
	}
	return verifier.PipelineInvocation{Request: a, Response: b, Profile: c, StageMS: v.StageMS}, nil
}

// WritePipelineAudit reproduces the old auditJob invocation encoding one entry
// at a time, including lower-case hex and encoding/json's float formatting.
func WritePipelineAudit(w io.Writer, metadata []byte, source *jsonspool.Object) error {
	e := PipelineExecution{Source: source}
	transform := func(w io.Writer) error {
		if _, err := io.WriteString(w, "["); err != nil {
			return err
		}
		for i := 0; i < e.InvocationCount(); i++ {
			inv, err := e.ReadInvocation(i)
			if err != nil {
				return err
			}
			if i > 0 {
				if _, err = io.WriteString(w, ","); err != nil {
					return err
				}
			}
			raw, err := json.Marshal(invocationJSON{hex.EncodeToString(inv.Request), hex.EncodeToString(inv.Response), hex.EncodeToString(inv.Profile), inv.StageMS})
			if err != nil {
				return err
			}
			if _, err = w.Write(raw); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, "]")
		return err
	}
	return jsonspool.WriteMergeTransform(w, metadata, source, map[string]func(io.Writer) error{"invocations": transform}, "openings", "invocations", "failovers")
}

// decodeSpoolEvent applies the original pipelineEvent schema one field/array
// element at a time. Wrapping each value retains encoding/json's original
// struct/field error labels, without ever decoding the complete result array.
func decodeSpoolEvent(source *jsonspool.Object) (pipelineEvent, error) {
	var event pipelineEvent
	var firstError error
	for _, field := range source.Ordered {
		name := strings.ToLower(field.Name)
		switch name {
		case "type", "text", "receipt", "stage_receipt_ref", "invocations", "openings", "failovers":
			source.Fields[name] = field.Span
			if field.Array {
				source.Arrays[name] = field.Entries
			} else {
				delete(source.Arrays, name)
			}
		}
		switch name {
		case "type", "text", "receipt", "stage_receipt_ref":
			key, _ := json.Marshal(field.Name)
			r := io.MultiReader(strings.NewReader("{"+string(key)+":"), source.Reader(field.Span), strings.NewReader("}"))
			if err := json.NewDecoder(r).Decode(&event); err != nil && firstError == nil {
				firstError = err
			}
		case "invocations":
			if field.Array {
				for _, span := range field.Entries {
					var single pipelineEvent
					r := io.MultiReader(strings.NewReader(`{"invocations":[`), source.Reader(span), strings.NewReader("]}"))
					if err := json.NewDecoder(r).Decode(&single); err != nil && firstError == nil {
						firstError = err
					}
				}
			} else {
				var single pipelineEvent
				r := io.MultiReader(strings.NewReader(`{"invocations":`), source.Reader(field.Span), strings.NewReader("}"))
				if err := json.NewDecoder(r).Decode(&single); err != nil && firstError == nil {
					firstError = err
				}
			}
		}
	}
	return event, firstError
}
