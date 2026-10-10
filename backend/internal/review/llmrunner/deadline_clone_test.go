package llmrunner_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

func TestStart_DeadlineDuringCloneIsErrDeadline(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	model := &fakeModel{}
	runner := newRunner(model)
	runner.SetRemote(srv.URL + "/o/r.git")
	runner.SetTimeout(300 * time.Millisecond)

	_, err := runner.Start(t.Context(), testRequest(strings.Repeat("a", 40)))
	if !errors.Is(err, pipeline.ErrTimeout) {
		t.Fatalf("Start() = %v, want errors.Is pipeline.ErrTimeout", err)
	}
	if len(model.calls) != 0 {
		t.Errorf("model saw %d calls, want 0", len(model.calls))
	}
}
