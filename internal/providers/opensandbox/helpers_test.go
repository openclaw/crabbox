package opensandbox

func (c *sdkOpenSandboxClient) handleCommandEvent(event commandStreamEvent) (commandEventResult, error) {
	return c.handleCommandEventWithState(event, &commandOutputState{})
}
