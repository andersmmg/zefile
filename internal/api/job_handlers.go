package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/krishna2206/zefile/internal/job"
)

type jobResponse struct {
	ID         int64   `json:"id"`
	Type       string  `json:"type"`
	Status     string  `json:"status"`
	Progress   float64 `json:"progress"`
	BytesDone  int64   `json:"bytes_done"`
	BytesTotal int64   `json:"bytes_total"`
	Error      string  `json:"error,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

func toJobResponse(j job.Job) jobResponse {
	out := jobResponse{
		ID:         j.ID,
		Type:       j.Type,
		Status:     j.Status,
		Progress:   j.Progress,
		BytesDone:  j.BytesDone,
		BytesTotal: j.BytesTotal,
		Error:      j.Error,
		CreatedAt:  j.CreatedAt.UTC(),
	}
	if !j.FinishedAt.IsZero() {
		finished := j.FinishedAt.UTC()
		out.FinishedAt = &finished
	}
	return out
}

type jobListResponse struct {
	Jobs []jobResponse `json:"jobs"`
}

// handleJobList returns the caller's recent background jobs.
func (s *Server) handleJobList(w http.ResponseWriter, r *http.Request) {
	c, ok := callerFrom(r.Context())
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return
	}
	jobs, err := s.jobs.ListFor(r.Context(), c.user.ID, c.user.IsAdmin, 50)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := make([]jobResponse, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toJobResponse(j))
	}
	writeJSON(w, r, http.StatusOK, jobListResponse{Jobs: out})
}

func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	c, ok := callerFrom(r.Context())
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, CodeBadRequest, "Bad id", "The job id must be a number.")
		return
	}
	j, err := s.jobs.GetFor(r.Context(), id, c.user.ID, c.user.IsAdmin)
	if err != nil {
		if errors.Is(err, job.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, CodeNotFound, "No such job", "This job is unknown.")
			return
		}
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toJobResponse(j))
}

func (s *Server) jobAction(w http.ResponseWriter, r *http.Request, act func(context.Context, int64) error) {
	c, ok := callerFrom(r.Context())
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated, "Not signed in", "")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, CodeBadRequest, "Bad id", "The job id must be a number.")
		return
	}
	if err := act(r.Context(), id); err != nil {
		if errors.Is(err, job.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, CodeNotFound, "No such job", "This job is unknown.")
			return
		}
		if errors.Is(err, job.ErrNotRunning) {
			writeProblem(w, r, http.StatusConflict, CodeConflict, "Not running", "Only a running job can be paused.")
			return
		}
		writeError(w, r, err)
		return
	}
	j, err := s.jobs.GetFor(r.Context(), id, c.user.ID, c.user.IsAdmin)
	if err != nil {
		if errors.Is(err, job.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, CodeNotFound, "No such job", "This job is unknown.")
			return
		}
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toJobResponse(j))
}

func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, func(ctx context.Context, id int64) error {
		c, ok := callerFrom(ctx)
		if !ok {
			return job.ErrNotFound
		}
		return s.jobs.CancelFor(ctx, id, c.user.ID, c.user.IsAdmin)
	})
}

func (s *Server) handleJobPause(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, func(ctx context.Context, id int64) error {
		c, ok := callerFrom(ctx)
		if !ok {
			return job.ErrNotFound
		}
		return s.jobs.PauseFor(ctx, id, c.user.ID, c.user.IsAdmin)
	})
}

func (s *Server) handleJobResume(w http.ResponseWriter, r *http.Request) {
	s.jobAction(w, r, func(ctx context.Context, id int64) error {
		c, ok := callerFrom(ctx)
		if !ok {
			return job.ErrNotFound
		}
		return s.jobs.ResumeFor(ctx, id, c.user.ID, c.user.IsAdmin)
	})
}
