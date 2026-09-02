package cli

// commandAgentRun runs the agent's poll/snapshot loop. Implemented in Task 16.
type commandAgentRun struct{}

func (c *commandAgentRun) setup(_ advancedAppServices, _ commandParent) {}
