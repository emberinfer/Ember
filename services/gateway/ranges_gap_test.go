package gateway

import (
	"ember/services/scheduler"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPipelineGapPassThrough(t *testing.T) {
	g := &Gateway{TotalLayers: 28, Pipeline: &PipelineConfig{Python: "unused", ModelPath: "unused", SchedulerKeyFile: "unused", DynamicRanges: true, MinStageLayers: 1}, Candidates: func() []scheduler.Candidate { return nil }}
	_, err := scheduler.PlanRanges(scheduler.Request{MinStageLayers: 1}, nil, 28)
	w := httptest.NewRecorder()
	g.servePipeline(w, httptest.NewRequest("POST", "/", nil), request{}, "", "", "")
	if w.Code != 503 || strings.TrimSuffix(w.Body.String(), "\n") != err.Error() {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
