package api

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/mail"
)

// Mail-by-bead lookup: the dashboard bead detail wants the last message from
// the implementing agent for a closed issue (owner request 2026-10-13).
// Executor READY/BLOCKED mail carries the bead id in subject or body
// ("READY: execute al6lr4 — ...", "BLOCKED: agent-forge-7slcv ...") and the
// sender identifies the executor session, so a substring match over the city
// mail store is the honest join available today. Fail-open per provider like
// GET /mail (a partial list beats a 503), newest-first, bounded.

const (
	mailByBeadDefaultLimit = 5
	mailByBeadMaxLimit     = 25
	// executorMailSenderMarker matches both pool (rig--tpl__agentforge-executor-N-pool)
	// and adhoc (rig/tpl.agentforge-executor-adhoc-<hex>) sender forms.
	executorMailSenderMarker = "agentforge-executor"
)

// filterMessagesByBead is the pure matcher (unit-tested): a message matches
// when the bead id occurs in the subject or the body, and — under
// kind=executor — the sender carries the executor marker. Newest first;
// equal timestamps break ties on id so pagination is deterministic.
func filterMessagesByBead(msgs []mail.Message, beadID, kind string) []mail.Message {
	beadID = strings.TrimSpace(beadID)
	if beadID == "" {
		return nil
	}
	executorOnly := strings.TrimSpace(kind) == "executor"
	var out []mail.Message
	for _, m := range msgs {
		if executorOnly && !strings.Contains(m.From, executorMailSenderMarker) {
			continue
		}
		if strings.Contains(m.Subject, beadID) || strings.Contains(m.Body, beadID) {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			// Same direction as the list routes' order contract
			// (created_at DESC, id DESC): newest first, id DESC tie-break.
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// collectMailByBead sweeps every city mail provider's All("") traffic (the
// same read the /mail all-traffic view rides), matches, and merges. A single
// provider failure degrades to a partial list; total failure propagates to
// the caller as an error condition to report.
func (s *Server) collectMailByBead(ctx context.Context, providers map[string]mail.Provider, beadID, kind string) ([]mail.Message, []string, bool) {
	var allMsgs []mail.Message
	var partialErrs []string
	partialStoreSlow := false
	for _, res := range withMailProviderReadDeadline(ctx, providers, func(provider mail.Provider) ([]mail.Message, error) {
		return provider.All("")
	}) {
		if res.err != nil {
			var timeoutErr *mailReadTimeoutError
			if errors.As(res.err, &timeoutErr) {
				partialStoreSlow = true
			}
			partialErrs = append(partialErrs, "mail provider "+res.name+": "+res.err.Error())
			continue
		}
		// Tag before matching so a hit carries its provider, matching the
		// /mail list contract (rig shown in the UI).
		allMsgs = append(allMsgs, tagRig(res.value, res.name)...)
	}
	return filterMessagesByBead(allMsgs, beadID, kind), partialErrs, partialStoreSlow
}

// humaHandleMailByBead is the Huma-typed handler for GET /v0/city/{city}/mail/by-bead/{beadID}.
func (s *Server) humaHandleMailByBead(ctx context.Context, input *MailByBeadInput) (*MailListOutput, error) {
	cityStore := s.state.CityBeadStore()
	if err := cacheLiveOr503(cityStore); err != nil {
		return nil, err
	}

	limit := mailByBeadDefaultLimit
	if input.Limit > 0 {
		limit = input.Limit
		if limit > mailByBeadMaxLimit {
			limit = mailByBeadMaxLimit
		}
	}

	providers := s.state.MailProviders()
	matched, partialErrs, partialStoreSlow := s.collectMailByBead(ctx, providers, input.BeadID, input.Kind)
	if len(partialErrs) == len(providers) && len(providers) > 0 {
		return nil, allMailProvidersFailedError(partialErrs, partialStoreSlow)
	}
	if matched == nil {
		matched = []mail.Message{}
	}
	// Total counts every match BEFORE truncation (the /mail semantics) so a
	// consumer knows when the bounded single page clipped the result.
	total := len(matched)
	if total > limit {
		matched = matched[:limit]
	}
	return &MailListOutput{
		Index:     s.latestIndex(),
		CacheAgeS: cacheAgeSeconds(cityStore),
		Body: MailListBody{
			Items:         matched,
			Total:         total,
			Partial:       len(partialErrs) > 0,
			PartialErrors: partialErrs,
		},
	}, nil
}
