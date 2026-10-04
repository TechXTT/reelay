package api

import "net/http"

type trialVoteRequest struct {
	Decision string `json:"decision"`
	Rating   int    `json:"rating"`
}

type trialPlaybackRequest struct {
	ServerID string `json:"server_id"`
	UserID   string `json:"user_id"`
	Episodes []struct {
		RequestID int64 `json:"request_id"`
		Season    int   `json:"season"`
		Number    int   `json:"number"`
	} `json:"episodes"`
}

func (s *Server) handleTrials(w http.ResponseWriter, r *http.Request) error {
	serverID, userID, err := userScope(r)
	if err != nil {
		return err
	}
	trials, err := s.store.Requests().ActiveTrials(r.Context(), serverID, userID)
	if err != nil {
		return err
	}
	return reply(w, r, http.StatusOK, map[string]any{"items": trials})
}

func (s *Server) handleTrialVote(w http.ResponseWriter, r *http.Request, id int64) error {
	body, err := decodeJSON[trialVoteRequest](r)
	if err != nil {
		return err
	}
	if (body.Decision != "continue" && body.Decision != "stop") || body.Rating < 1 || body.Rating > 5 {
		return BadRequest("choose continue or stop and a rating from 1 to 5")
	}
	if err := s.store.Requests().VoteTrial(r.Context(), id, body.Decision, body.Rating); err != nil {
		return Conflict("trial vote could not be completed").WithCause(err)
	}
	trial, err := s.store.Requests().Trial(r.Context(), id)
	if err != nil {
		return err
	}
	if body.Decision == "continue" {
		s.trigger("metadata", "search")
	}
	s.trigger("recommendations")
	s.publishRequestsUpdated(trial.ServerID, trial.UserID)
	return reply(w, r, http.StatusOK, map[string]any{"trial": trial})
}

func (s *Server) handleTrialPlayback(w http.ResponseWriter, r *http.Request) error {
	body, err := decodeJSON[trialPlaybackRequest](r)
	if err != nil {
		return err
	}
	if body.ServerID == "" || body.UserID == "" || len(body.Episodes) > 400 {
		return BadRequest("trial playback needs server_id, user_id, and at most 400 episodes")
	}
	for _, episode := range body.Episodes {
		if err := s.store.Requests().MarkTrialWatched(r.Context(), episode.RequestID, body.ServerID, body.UserID, episode.Season, episode.Number); err != nil {
			return Conflict("trial episode could not be marked watched").WithCause(err)
		}
	}
	s.publishRequestsUpdated(body.ServerID, body.UserID)
	return reply(w, r, http.StatusOK, map[string]any{"updated": len(body.Episodes)})
}
