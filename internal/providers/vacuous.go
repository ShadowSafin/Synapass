package providers

import (
	"net/http"

	"github.com/shadowsafin/synapass/internal/domain"
)

// VacuousToolResponse reports whether a request that carried tools came back
// with nothing an agent can act on: no answer text, no tool calls, no
// refusal.
//
// Some OpenAI-compatible backends accept the tools shape and then return an
// empty message (thinking tokens spent, nothing emitted). Serving that as a
// success stalls every agent loop: the client sees finish_reason stop with no
// content and waits for an action that never comes. Reporting it as an
// upstream failure instead lets the executor fail over to a backend that can
// serve tools (non-streaming), or surfaces an honest stream error the client
// can display (streaming), and the health tracker learns from both.
//
// Plain-chat responses are deliberately excluded: an empty reply without
// tools is served as-is, so this changes nothing for non-agentic traffic.
func VacuousToolResponse(params *domain.ChatCompletionRequest, resp *Response) bool {
	if params == nil || len(params.Tools) == 0 {
		return false
	}
	if resp == nil || len(resp.Choices) == 0 {
		return true
	}
	msg := resp.Choices[0].Message
	if msg == nil {
		return true
	}
	return msg.Text() == "" && len(msg.ToolCalls) == 0 && msg.Refusal == ""
}

// NewVacuousToolResponseError renders a vacuous tool response as the
// retryable upstream failure the executor acts on.
func NewVacuousToolResponseError(adapterName, model string, attempt, toolCount int) error {
	return domain.Errorf(domain.ErrCodeUpstream,
		"provider %s returned an empty completion for model %q on a request carrying %d tool(s): no answer text, no tool calls, no refusal",
		adapterName, model, toolCount).
		WithStatus(http.StatusBadGateway).
		WithProvider(adapterName, model, attempt)
}

// rejectVacuousToolResponse returns nil when resp is servable and an upstream
// error when a tool-carrying request came back vacuous. Adapters call it on
// every completion path (streaming and non-streaming) so the rule holds for
// every provider kind.
func rejectVacuousToolResponse(adapterName string, req *Request, resp *Response) error {
	if !VacuousToolResponse(req.Params, resp) {
		return nil
	}
	count := 0
	if req.Params != nil {
		count = len(req.Params.Tools)
	}
	return NewVacuousToolResponseError(adapterName, req.Model, 1, count)
}
