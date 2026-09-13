package llm

import (
	"errors"
	"fmt"
)

// ErrIncompleteGeneration identifies retryable unfinished generation, never an
// executable partial response. Explicit filtering/provider failures stay errors.
var ErrIncompleteGeneration = errors.New("model generation incomplete")

func incompleteResponse(r responsesResponse) error {
	reason := r.failureDetail()
	if r.Error.Message == "" && r.Error.Code == "" && (r.IncompleteDetails.Reason == "" || r.IncompleteDetails.Reason == "max_output_tokens") {
		return fmt.Errorf("%w: %s", ErrIncompleteGeneration, reason)
	}
	return fmt.Errorf("openai response incomplete: %s", reason)
}

func unfinishedChat(reason string) error {
	if reason == "length" {
		return fmt.Errorf("%w: finish_reason=length", ErrIncompleteGeneration)
	}
	return fmt.Errorf("openai native chat did not complete: %s", reason)
}
